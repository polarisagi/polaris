package checkpoint_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/security/checkpoint"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestTrigger_ConditionsAndThrottle(t *testing.T) {
	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	projectDir := filepath.Join(tmpDir, "project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	testFile := filepath.Join(projectDir, "a.txt")
	_ = os.WriteFile(testFile, []byte("init"), 0o644)

	store, err := checkpoint.NewStore(dataDir, projectDir, 10)
	if err != nil {
		t.Fatal(err)
	}

	trigger := checkpoint.NewTrigger(store, projectDir, 2)
	ctx := context.Background()

	// 1. 只读操作不打快照
	err = trigger.MaybeSnapshot(ctx, "read_file", types.CapReadOnly, nil, []byte(`{"path":"a.txt"}`), types.TaintLow)
	if err != nil {
		t.Fatal(err)
	}
	if trigger.TurnSnapshotCount() != 0 {
		t.Errorf("expected 0 snapshots for read operation, got %d", trigger.TurnSnapshotCount())
	}

	// 2. 会话首个写操作：触发快照
	_ = os.WriteFile(testFile, []byte("mod1"), 0o644)
	err = trigger.MaybeSnapshot(ctx, "write_file", types.CapWriteLocal, []types.SideEffect{types.SideFileWrite}, []byte(`{"path":"a.txt"}`), types.TaintLow)
	if err != nil {
		t.Fatal(err)
	}
	if trigger.TurnSnapshotCount() != 1 {
		t.Errorf("expected 1 snapshot after first write, got %d", trigger.TurnSnapshotCount())
	}

	// 3. 删除操作：触发快照
	_ = os.WriteFile(testFile, []byte("mod2"), 0o644)
	err = trigger.MaybeSnapshot(ctx, "bash", types.CapWriteLocal, []types.SideEffect{types.SideProcessSpawn}, []byte(`{"command":"rm a.txt"}`), types.TaintLow)
	if err != nil {
		t.Fatal(err)
	}
	if trigger.TurnSnapshotCount() != 2 {
		t.Errorf("expected 2 snapshots after delete, got %d", trigger.TurnSnapshotCount())
	}

	// 4. 超出节流上限（throttleMax=2）：跳过快照
	_ = os.WriteFile(testFile, []byte("mod3"), 0o644)
	err = trigger.MaybeSnapshot(ctx, "write_file", types.CapWriteLocal, []types.SideEffect{types.SideFileWrite}, []byte(`{"path":"a.txt"}`), types.TaintHigh)
	if err != nil {
		t.Fatal(err)
	}
	if trigger.TurnSnapshotCount() != 3 { // 计数器仍递增以记录尝试
		t.Errorf("expected turn count to be 3, got %d", trigger.TurnSnapshotCount())
	}

	// 5. 重置回合后继续打快照
	trigger.ResetTurn()
	if trigger.TurnSnapshotCount() != 0 {
		t.Errorf("expected 0 after reset, got %d", trigger.TurnSnapshotCount())
	}
}
