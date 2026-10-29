package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// GR-3-001：Close 必须排空残余并落盘；Close 之后的 Submit 返回哨兵而不是 panic。
func TestDatabaseWriter_CloseDrainsAndRejects(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	dw := NewDatabaseWriter(db, nil)
	done := make(chan struct{})
	go func() { dw.Run(context.Background()); close(done) }()

	// Ensure Run has started and is processing before we submit the batch and Close.
	res := make(chan error, 1)
	if err := dw.Submit(context.Background(), &MutationIntent{Table: "tasks", Operation: "insert", Key: []byte("ping"), ResultCh: res}); err != nil {
		t.Fatalf("submit ping: %v", err)
	}
	<-res

	for i := 0; i < 9; i++ { // submit 9 more to make it 10 total
		intent := &MutationIntent{Table: "tasks", Operation: "insert",
			Key: []byte{byte('a' + i)}, Payload: []byte("p")}
		if err := dw.Submit(context.Background(), intent); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}
	dw.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after Close")
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&n); err != nil || n != 10 {
		t.Fatalf("rows=%d err=%v, want 10 flushed on Close", n, err)
	}
	err := dw.Submit(context.Background(), &MutationIntent{Table: "tasks", Operation: "insert", Key: []byte("z")})
	if !errors.Is(err, ErrDatabaseWriterClosed) {
		t.Fatalf("submit after close = %v, want ErrDatabaseWriterClosed", err)
	}
	dw.Close() // 幂等
}

// ctx 取消时最后一批不得因"已取消的 ctx"而整批失败。
func TestDatabaseWriter_CancelStillFlushes(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()
	dw := NewDatabaseWriter(db, nil)
	ctx, cancel := context.WithCancel(context.Background())
	res := make(chan error, 1)
	// 先入队、后启动 Run 并立即取消，保证该 intent 走的是退出路径的 finalFlush。
	if err := dw.Submit(context.Background(), &MutationIntent{Table: "tasks", Operation: "insert",
		Key: []byte("k"), Payload: []byte("p"), ResultCh: res}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	cancel()
	dw.Run(ctx)
	if err := <-res; err != nil {
		t.Fatalf("final flush failed: %v", err)
	}
}
