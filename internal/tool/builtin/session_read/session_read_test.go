package session_read

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func newDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE chat_messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, role TEXT NOT NULL,
		content TEXT NOT NULL, created_at TEXT NOT NULL DEFAULT '2026-10-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 10; i++ {
		if _, err := db.Exec(`INSERT INTO chat_messages(session_id, role, content) VALUES ('s1','user',?)`, fmt.Sprintf("m%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestSessionRead_AroundMessage(t *testing.T) {
	db := newDB(t)
	out, err := MakeSessionReadFn(db)(context.Background(), []byte(`{"message_id":5,"around":2}`))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"message=3", "message=7", "message=5 user 2026-10-01T00:00:00Z <== hit"} {
		if !strings.Contains(s, want) {
			t.Fatalf("缺少 %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "message=2 ") || strings.Contains(s, "message=8 ") {
		t.Fatalf("超出 around 范围:\n%s", s)
	}
}

func TestSessionRead_LatestOfSessionAndErrors(t *testing.T) {
	db := newDB(t)
	fn := MakeSessionReadFn(db)
	out, err := fn(context.Background(), []byte(`{"session_id":"s1","around":2}`))
	if err != nil || !strings.Contains(string(out), "message=10") || strings.Contains(string(out), "message=6 ") {
		t.Fatalf("应返回最近 4 条: %v\n%s", err, out)
	}
	if _, err := fn(context.Background(), []byte(`{}`)); err == nil {
		t.Fatal("缺参应报错")
	}
	if _, err := fn(context.Background(), []byte(`{"message_id":999}`)); err == nil {
		t.Fatal("不存在的消息应报错")
	}
	if _, err := fn(context.Background(), []byte(`{"message_id":5,"session_id":"other"}`)); err == nil {
		t.Fatal("会话不一致应报错")
	}
}
