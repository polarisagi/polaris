package session_search

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
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
	ddl, err := schema.FS.ReadFile("054_messages_trigram_fts.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatalf("054 ddl: %v", err)
	}
	for _, m := range [][2]string{
		{"s1", "我们讨论过数据库迁移方案，决定先做双写再切换"},
		{"s1", "Postgres migration plan: dual write first"},
		{"s2", "今天聊聊天气"},
		{"cur", "当前会话里也提到了数据库迁移"},
	} {
		if _, err := db.Exec(`INSERT INTO chat_messages(session_id, role, content) VALUES (?, 'user', ?)`, m[0], m[1]); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func run(t *testing.T, db *sql.DB, ctx context.Context, in string) string {
	t.Helper()
	out, err := MakeSessionSearchFn(db)(ctx, []byte(in))
	if err != nil {
		t.Fatalf("search %s: %v", in, err)
	}
	return string(out)
}

func TestSessionSearch_LongCJKQueryUsesTrigramAndExcludesCurrent(t *testing.T) {
	db := newDB(t)
	ctx := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, "cur")
	got := run(t, db, ctx, `{"query":"数据库迁移"}`)
	if !strings.Contains(got, "session=s1") || strings.Contains(got, "session=cur") {
		t.Fatalf("应命中 s1 且排除当前会话:\n%s", got)
	}
}

func TestSessionSearch_TwoRuneCJKFallsBackToLike(t *testing.T) {
	db := newDB(t)
	ctx := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, "cur")
	got := run(t, db, ctx, `{"query":"天气"}`)
	if !strings.Contains(got, "session=s2") {
		t.Fatalf("2 字中文应经 LIKE 命中:\n%s", got)
	}
}

func TestSessionSearch_ExplicitSessionIncludesCurrent(t *testing.T) {
	db := newDB(t)
	ctx := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, "cur")
	got := run(t, db, ctx, `{"query":"数据库迁移","session_id":"cur"}`)
	if !strings.Contains(got, "session=cur") {
		t.Fatalf("显式传入当前会话应可检索:\n%s", got)
	}
}

func TestSessionSearch_CaseInsensitiveAndNoMatch(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	if got := run(t, db, ctx, `{"query":"DUAL WRITE"}`); !strings.Contains(got, "message=2") {
		t.Fatalf("trigram 应大小写不敏感:\n%s", got)
	}
	if got := run(t, db, ctx, `{"query":"不存在的词汇xyz"}`); got != "no matches" {
		t.Fatalf("got %q", got)
	}
	if _, err := MakeSessionSearchFn(db)(ctx, []byte(`{"query":"  "}`)); err == nil {
		t.Fatal("空查询应报错")
	}
}

func TestSessionSearch_QuoteInQueryDoesNotBreakFTS(t *testing.T) {
	db := newDB(t)
	if _, err := MakeSessionSearchFn(db)(context.Background(), []byte(`{"query":"say \"hello\" now"}`)); err != nil {
		t.Fatalf("带引号查询不应报 FTS 语法错: %v", err)
	}
}
