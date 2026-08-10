package graph

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/memory/testutil"
)

func TestEdgeWeightManager(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewMockStore()

	_, err := store.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS world_model_edges (
			edge_id TEXT PRIMARY KEY,
			storage_strength REAL DEFAULT 1.0,
			retrieval_strength REAL DEFAULT 1.0,
			last_accessed_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		t.Fatalf("failed to create test table: %v", err)
	}

	ewm := NewEdgeWeightManager(store)

	w, err2 := ewm.ReinforcePath(ctx, "edge1", 0.5)
	if err2 != nil {
		t.Fatalf("expected reinforce to succeed, got err: %v", err2)
	}
	if w <= 0.5 {
		t.Fatal("expected weight to increase")
	}

	// 验证落盘
	var ss float64
	err = store.QueryRowContext(ctx, "SELECT storage_strength FROM world_model_edges WHERE edge_id = 'edge1'").Scan(&ss)
	if err != nil {
		t.Fatalf("failed to query edge1: %v", err)
	}
	if ss != 1.05 {
		t.Errorf("expected storage_strength 1.05, got %v", ss)
	}

	w, err2 = ewm.ReinforcePath(ctx, "edge2", 1.0)
	if err2 != nil {
		t.Fatalf("expected reinforce to succeed, got err: %v", err2)
	}
	if w > 1.0 {
		t.Fatal("weight exceeded 1.0")
	}

	w2 := ewm.DecayUnused(1.0, time.Now().Add(-60*24*time.Hour))
	if w2 >= 1.0 {
		t.Fatal("expected weight to decay")
	}

	w3 := ewm.DecayUnused(1.0, time.Now().Add(1*time.Hour))
	if w3 != 1.0 {
		t.Fatal("expected no decay for future/present")
	}

	err = ewm.FeedbackCalibrate(ctx, []string{"edge1"})
	if err != nil {
		t.Fatal(err)
	}
	err = store.QueryRowContext(ctx, "SELECT storage_strength FROM world_model_edges WHERE edge_id = 'edge1'").Scan(&ss)
	if err == nil && ss < 1.06 { // 1.05 + 0.03 = 1.08
		t.Errorf("expected storage_strength > 1.06 after calibrate, got %v", ss)
	}

	// 构造一条低强度的边用于 Prune 测试
	_, _ = store.ExecContext(ctx, "INSERT INTO world_model_edges (edge_id, storage_strength, retrieval_strength) VALUES ('edge3', 0.1, 0.5)") // 0.1 * 0.5 = 0.05 < 0.1
	err = ewm.PeriodicPrune(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// edge3 应该被删除
	var c int
	_ = store.QueryRowContext(ctx, "SELECT COUNT(*) FROM world_model_edges WHERE edge_id = 'edge3'").Scan(&c)
	if c != 0 {
		t.Error("expected edge3 to be pruned")
	}
}

// TestEdgeWeightManager_ReinforcePath_WriteFailureReturnsError 验证 DB 写入失败时
// ReinforcePath 返回未变更的原始权重与非 nil error，而不是一个只存在于内存里、
// 与库中真实 strength 背离的"看起来已生效"的值（ADR-0094 决策四）。
func TestEdgeWeightManager_ReinforcePath_WriteFailureReturnsError(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewMockStore()
	// 故意不建表：INSERT INTO world_model_edges 必然因表不存在而失败。
	ewm := NewEdgeWeightManager(store)

	const original = 0.42
	w, err := ewm.ReinforcePath(ctx, "edge-missing-table", original)
	if err == nil {
		t.Fatal("expected error when underlying table does not exist, got nil")
	}
	if w != original {
		t.Fatalf("expected unchanged weight %v on write failure, got %v", original, w)
	}
}
