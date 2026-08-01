package graphrag

import (
	"context"
	"testing"
)

// TestGraphWriter_UpsertEntity_SemanticDBWriteFailure_ReturnsError_S02 验证
// 阶段02修复（GR-7-005）：写入 semantic_entities 失败时 UpsertEntity 必须向上
// 返回错误，不得静默吞没继续执行到 gw.bus.Submit（该 bus 在本测试中为 nil，
// 若错误被吞没会直接 panic——这本身就是修复前行为脆弱的证明）。
// 回归锚点：修复前 `_, _ = gw.semanticDB.ExecContext(...)` 吞没错误，UpsertEntity
// 会继续认为该实体已"跳过或成功"，图谱因此缺节点且完全不可观测。
func TestGraphWriter_UpsertEntity_SemanticDBWriteFailure_ReturnsError_S02(t *testing.T) {
	db := setupSemanticDB(t)
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TRIGGER reject_entity_insert BEFORE INSERT ON semantic_entities
		BEGIN SELECT RAISE(ABORT, 'simulated semantic_entities insert failure'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	gw := &GraphWriter{
		bus:        nil, // 若错误被吞没，执行会继续到这里并 panic
		fetcher:    nil,
		semanticDB: &testQuerier{db: db},
	}

	e := &Entity{
		ID:          "e-test-fail",
		Name:        "FailEntity",
		Type:        "Project",
		Embedding:   []float32{1.0, 0.0, 0.0},
		SyncVersion: 1,
	}

	err := gw.UpsertEntity(context.Background(), e)
	if err == nil {
		t.Fatal("expected error when semantic_entities insert fails, got nil")
	}

	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM semantic_entities WHERE name='FailEntity'`).Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 0 {
		t.Errorf("expected no row written (insert rejected by trigger), got %d", count)
	}
}
