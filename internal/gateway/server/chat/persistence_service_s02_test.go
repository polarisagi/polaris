package chat

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/store/repo"
)

// TestTouchSession_RespectsParentCancellation_S02 验证阶段02修复：TouchSession
// 内部超时 context 必须以调用方传入的 ctx 为父级派生，而不是像修复前那样用
// context.Background() 彻底斩断取消链路。父 ctx 已取消时，派生的 tctx 也应
// 立即处于 Done 状态，DB 调用应快速失败并返回错误，而不是无视取消信号继续
// 跑满 5s 超时。
func TestTouchSession_RespectsParentCancellation_S02(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE chat_sessions (
			id TEXT PRIMARY KEY, title TEXT, updated_at DATETIME
		)`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO chat_sessions (id, title) VALUES ('sess-cancel', 'x')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := &ChatPersistenceService{DB: db, ChatRepo: repo.NewSQLiteChatRepository(db)}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 调用前即取消，模拟客户端已断连场景

	err = svc.TouchSession(ctx, "sess-cancel")
	if err == nil {
		t.Fatal("expected error when parent ctx is already cancelled, got nil (context chain not propagated)")
	}
}

// TestTouchSession_NormalContext_Succeeds_S02 正常路径回归锚点：确保修复后
// 未取消的 ctx 仍能正常完成落库（防止上面的取消检测误伤正常场景）。
func TestTouchSession_NormalContext_Succeeds_S02(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	// :memory: 每条连接都是独立空库（无 cache=shared），池开出第二条即读到空表。
	db.SetMaxOpenConns(1)
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE chat_sessions (
			id TEXT PRIMARY KEY, title TEXT, updated_at DATETIME
		)`); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO chat_sessions (id, title) VALUES ('sess-ok', 'x')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	svc := &ChatPersistenceService{DB: db, ChatRepo: repo.NewSQLiteChatRepository(db)}

	if err := svc.TouchSession(context.Background(), "sess-ok"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
