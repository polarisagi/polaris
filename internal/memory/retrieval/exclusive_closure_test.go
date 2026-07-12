package retrieval_test

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/memory/retrieval"
	memstore "github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/memory/testutil"
	"github.com/polarisagi/polaris/pkg/types"
)

// insertEntityFull 与 cascade_invalidator_test.go 的 insertEntity 类似，但允许自定义
// entity_type（Jaccard 近似碰撞仅对 'user_preference' 类型启用，需要与 insertEntity
// 硬编码的 'Concept' 区分）。
func insertEntityFull(t *testing.T, ms *testutil.MockStore, id int64, entityType, name string) {
	t.Helper()
	now := time.Now().UnixMilli()
	_, err := ms.DB().Exec(
		`INSERT INTO semantic_entities(id, entity_type, name, status, created_at, updated_at) VALUES (?, ?, ?, 'active', ?, ?)`,
		id, entityType, name, now, now,
	)
	if err != nil {
		t.Fatalf("failed to insert entity %d: %v", id, err)
	}
}

func insertRelationFull(t *testing.T, ms *testutil.MockStore, sourceID, targetID int64, relType string) {
	t.Helper()
	_, err := ms.DB().Exec(
		`INSERT INTO semantic_relations(source_id, target_id, relation_type, created_at) VALUES (?, ?, ?, ?)`,
		sourceID, targetID, relType, time.Now().UnixMilli(),
	)
	if err != nil {
		t.Fatalf("failed to insert relation %d->%d: %v", sourceID, targetID, err)
	}
}

func entityStatusFull(t *testing.T, ms *testutil.MockStore, id int64) string {
	t.Helper()
	var status string
	if err := ms.DB().QueryRow(`SELECT status FROM semantic_entities WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatalf("failed to query status for %d: %v", id, err)
	}
	return status
}

// TestExclusiveWriter_ExactCollision_TriggersCascade 验证 GD-14-001 端到端链路：
// UpsertFactExclusive 遇到精确碰撞（同 entity_type+name 的活跃实体）时，标记旧实体
// superseded 后必须触发 CascadeInvalidator，使沿 semantic_relations 相连的下游实体
// 进入 pending_review。此前该链路仅在 handleExistingEntity 内部单元验证过 cascadeInv
// 字段被传入，未验证真正触发下游状态变更——本测试用真实 SemanticMem + 真实
// CascadeInvalidator 组合，覆盖 consolidation.upsertSemantic 的实际调用路径。
func TestExclusiveWriter_ExactCollision_TriggersCascade(t *testing.T) {
	ms := testutil.NewMockStore()
	semantic := memstore.NewSemanticMem(ms, &testutil.MockIntentSubmitter{})
	ctx := context.Background()

	insertEntityFull(t, ms, 1, "Concept", "seed")
	insertEntityFull(t, ms, 2, "Concept", "downstream")
	insertRelationFull(t, ms, 1, 2, "derived_from")

	cascadeInv := retrieval.NewCascadeInvalidator(ms.DB())
	writer := retrieval.NewExclusiveWriter(semantic, cascadeInv, ms.DB())

	newFact := &types.Entity{Type: "Concept", Name: "seed", Confidence: 0.9}
	if err := writer.UpsertFactExclusive(ctx, newFact, types.TaintNone); err != nil {
		t.Fatalf("UpsertFactExclusive failed: %v", err)
	}

	if got := entityStatusFull(t, ms, 1); got != "superseded" {
		t.Errorf("entity 1 (exact collision source): expected status superseded, got %q", got)
	}
	if got := entityStatusFull(t, ms, 2); got != "pending_review" {
		t.Errorf("entity 2 (cascade downstream): expected status pending_review, got %q", got)
	}
}

// TestExclusiveWriter_JaccardCollision_TriggersCascade 验证 GD-14-001 复核修复：
// 此前 supersedeSimilarPreferences（Jaccard 近似碰撞分支）只调用 MarkEntitySuperseded，
// 未触发级联失效，与精确碰撞分支行为不一致。现改为共用 supersedeAndCascade，本测试
// 验证 Jaccard 判定为同一偏好旧版本的实体被 superseded 后，其下游关联实体同样会
// 进入 pending_review。
func TestExclusiveWriter_JaccardCollision_TriggersCascade(t *testing.T) {
	ms := testutil.NewMockStore()
	semantic := memstore.NewSemanticMem(ms, &testutil.MockIntentSubmitter{})
	ctx := context.Background()

	// tokens: {user,likes,blue,theme,accent} vs {user,likes,blue,theme,design}
	// intersection=4, union=6 → Jaccard = 4/6 ≈ 0.667 > 0.6 阈值。
	insertEntityFull(t, ms, 10, "user_preference", "user likes blue theme accent")
	insertEntityFull(t, ms, 11, "Concept", "downstream_of_pref")
	insertRelationFull(t, ms, 10, 11, "configures")

	cascadeInv := retrieval.NewCascadeInvalidator(ms.DB())
	writer := retrieval.NewExclusiveWriter(semantic, cascadeInv, ms.DB())

	newFact := &types.Entity{Type: "user_preference", Name: "user likes blue theme design", Confidence: 0.9}
	if err := writer.UpsertFactExclusive(ctx, newFact, types.TaintNone); err != nil {
		t.Fatalf("UpsertFactExclusive failed: %v", err)
	}

	if got := entityStatusFull(t, ms, 10); got != "superseded" {
		t.Errorf("entity 10 (Jaccard collision source): expected status superseded, got %q", got)
	}
	if got := entityStatusFull(t, ms, 11); got != "pending_review" {
		t.Errorf("entity 11 (cascade downstream of Jaccard-superseded entity): expected status pending_review, got %q — this is exactly the GD-14-001 gap: Jaccard branch must cascade like the exact-collision branch", got)
	}
}

// TestExclusiveWriter_JaccardCollision_BelowThreshold_NoCascade 验证低于 0.6 阈值的
// 名称不会被误判为同一偏好旧版本，不触发 superseded/cascade（避免误报回归）。
func TestExclusiveWriter_JaccardCollision_BelowThreshold_NoCascade(t *testing.T) {
	ms := testutil.NewMockStore()
	semantic := memstore.NewSemanticMem(ms, &testutil.MockIntentSubmitter{})
	ctx := context.Background()

	insertEntityFull(t, ms, 20, "user_preference", "completely unrelated topic")
	insertEntityFull(t, ms, 21, "Concept", "downstream_of_pref_2")
	insertRelationFull(t, ms, 20, 21, "configures")

	cascadeInv := retrieval.NewCascadeInvalidator(ms.DB())
	writer := retrieval.NewExclusiveWriter(semantic, cascadeInv, ms.DB())

	newFact := &types.Entity{Type: "user_preference", Name: "dark mode enabled toggle", Confidence: 0.9}
	if err := writer.UpsertFactExclusive(ctx, newFact, types.TaintNone); err != nil {
		t.Fatalf("UpsertFactExclusive failed: %v", err)
	}

	if got := entityStatusFull(t, ms, 20); got != "active" {
		t.Errorf("entity 20 (dissimilar name): expected to remain active, got %q", got)
	}
	if got := entityStatusFull(t, ms, 21); got != "active" {
		t.Errorf("entity 21 (downstream of unrelated entity): expected to remain active, got %q", got)
	}
}
