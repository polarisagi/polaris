package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol/pb"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/pkg/substrate"
)

func TestEventLogger(t *testing.T) {
	// 准备临时的 SQLite 数据库
	tmpDir, err := os.MkdirTemp("", "polaris-eventlog-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "polaris.db")
	store, err := OpenSQLite(dbPath, schema.FS)
	if err != nil {
		t.Fatalf("OpenSQLite failed: %v", err)
	}
	defer store.Close()

	// 准备 MutationBus 和 EventLogger
	// OpenSQLite 会返回 *SQLiteStore，里面的 db 可以通过暴露出 GetDB 或者直接强制转换，或者我们在 store.go 里面加一个 DB() 方法。
	// 这里因为是同一个包，可以直接访问 store.db。
	writer := substrate.NewDatabaseWriter(store.db, nil)
	// 启动 worker
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go writer.Run(ctx)
	defer writer.Close()

	logger := NewSQLiteEventLog(writer)

	// 构建测试事件
	ev := &pb.Event{
		Id:             "01J5K...",
		Topic:          "agent.test",
		Actor:          "system",
		Type:           "system_init",
		Payload:        []byte(`{"test":"ok"}`),
		IdempotencyKey: "init:1",
		CreatedAt:      time.Now().UnixMilli(),
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()

	if err := logger.AppendEvent(ctx2, ev); err != nil {
		t.Fatalf("AppendEvent failed: %v", err)
	}

	// 验证落盘
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM events WHERE id = ?", ev.Id).Scan(&count); err != nil {
		t.Fatalf("QueryRow failed: %v", err)
	}
	if count != 1 {
		t.Errorf("Expected 1 event, got %d", count)
	}
}
