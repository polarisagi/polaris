package agentctx

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/protocol/schema"
)

func newPIIVaultTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, f := range []string{"016_preferences.sql", "046_task_pii_vault.sql"} {
		ddl, err := schema.FS.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(ddl)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	return db
}

func newTestVault(t *testing.T) (*SessionPIIVault, *sql.DB) {
	db := newPIIVaultTestDB(t)
	return NewSessionPIIVault(db, []byte("0123456789abcdef0123456789abcdef")), db
}

func TestSessionPIIVault_SnapshotLoadRoundTrip(t *testing.T) {
	ctx := context.Background()
	v, db := newTestVault(t)
	in := map[string]string{"raw_intent": "我的手机号 13800000000", "session_id": "s-1"}
	if err := v.Snapshot(ctx, "t1", in); err != nil {
		t.Fatal(err)
	}
	got, err := v.Load(ctx, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["raw_intent"] != in["raw_intent"] || got["session_id"] != "s-1" {
		t.Fatalf("往返不一致: %v", got)
	}
	// 落库的必须是密文，且不得进入 preferences。
	var enc string
	if err := db.QueryRow("SELECT enc_value FROM task_pii_vault WHERE task_id='t1' AND field='raw_intent'").Scan(&enc); err != nil {
		t.Fatal(err)
	}
	if enc == in["raw_intent"] || enc == "" {
		t.Fatalf("enc_value 应为密文: %q", enc)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM preferences").Scan(&n); err != nil || n != 0 {
		t.Fatalf("preferences 不应出现任何 PII 行: n=%d err=%v", n, err)
	}
	if other, _ := v.Load(ctx, "t2"); len(other) != 0 {
		t.Fatalf("其他任务不应读到快照: %v", other)
	}
}

func TestSessionPIIVault_ExpiredNotReturned(t *testing.T) {
	ctx := context.Background()
	v, db := newTestVault(t)
	if err := v.Snapshot(ctx, "t1", map[string]string{"raw_intent": "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE task_pii_vault SET expired_at = ?", time.Now().UnixMilli()-1000); err != nil {
		t.Fatal(err)
	}
	got, err := v.Load(ctx, "t1")
	if err != nil || len(got) != 0 {
		t.Fatalf("过期行不应返回: %v %v", got, err)
	}
	if err := v.RestoreFromSnapshot(ctx, "t1"); err != nil {
		t.Fatalf("快照缺失只告警不报错: %v", err)
	}
}

func TestSessionPIIVault_SecureZeroClears(t *testing.T) {
	ctx := context.Background()
	v, db := newTestVault(t)
	_ = v.Snapshot(ctx, "t1", map[string]string{"raw_intent": "a"})
	_ = v.Snapshot(ctx, "t2", map[string]string{"raw_intent": "b"})
	if err := v.SecureZero(ctx, "t1"); err != nil {
		t.Fatal(err)
	}
	var n1, n2 int
	_ = db.QueryRow("SELECT COUNT(*) FROM task_pii_vault WHERE task_id='t1'").Scan(&n1)
	_ = db.QueryRow("SELECT COUNT(*) FROM task_pii_vault WHERE task_id='t2'").Scan(&n2)
	if n1 != 0 || n2 != 1 {
		t.Fatalf("SecureZero 只清目标任务: t1=%d t2=%d", n1, n2)
	}
}
