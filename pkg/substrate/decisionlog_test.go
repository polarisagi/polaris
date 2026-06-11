package substrate

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/pkg/substrate/storage"
)

func TestDecisionLogger(t *testing.T) {
	// 准备临时的 SQLite 数据库
	tmpDir, err := os.MkdirTemp("", "polaris-decisionlog-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp failed: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "polaris.db")
	store, err := storage.OpenSQLite(dbPath, schema.FS)
	if err != nil {
		t.Fatalf("OpenSQLite failed: %v", err)
	}
	defer store.Close()

	// 准备 MutationBus 和 DecisionLogger
	writer := NewDatabaseWriter(store.DB(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go writer.Run(ctx)
	defer writer.Close()

	logger := NewSQLiteDecisionLog(writer)

	// 构建测试决策记录
	ctxJSON, _ := json.Marshal(map[string]any{"task": "test"})
	altJSON, _ := json.Marshal([]string{"optionA", "optionB"})

	entry := &protocol.DecisionLogEntry{
		SessionID:    "sess-001",
		AgentID:      "agent-007",
		DecisionType: "route_model",
		Context:      ctxJSON,
		Choice:       "deepseek-v4-flash",
		Alternatives: altJSON,
		Reason:       "best balance",
		// Outcome: 初始可为空
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()

	if err := logger.AppendDecision(ctx2, entry); err != nil {
		t.Fatalf("AppendDecision failed: %v", err)
	}

	// 验证落盘
	var count int
	if err := store.DB().QueryRow("SELECT COUNT(*) FROM decision_log WHERE session_id = ? AND decision_type = ?", "sess-001", "route_model").Scan(&count); err != nil {
		t.Fatalf("QueryRow failed: %v", err)
	}
	if count != 1 {
		t.Errorf("Expected 1 decision log entry, got %d", count)
	}
}
