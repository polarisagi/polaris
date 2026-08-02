package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/polarisagi/polaris/internal/memory/graph"
	"github.com/polarisagi/polaris/internal/memory/testutil"
	"github.com/polarisagi/polaris/pkg/types"
)

// setupRelationFixture 建两个实体（供关系测试使用），返回其 DBID。
func setupRelationFixture(t *testing.T, mem *SemanticMem, ctx context.Context) (fromDBID, toDBID int64) {
	t.Helper()
	if err := mem.UpsertFact(ctx, types.Entity{Name: "Zhang San", Type: "person"}, types.TaintNone); err != nil {
		t.Fatalf("UpsertFact from failed: %v", err)
	}
	if err := mem.UpsertFact(ctx, types.Entity{Name: "Acme Corp", Type: "org"}, types.TaintNone); err != nil {
		t.Fatalf("UpsertFact to failed: %v", err)
	}
	from, err := mem.GetEntity(ctx, "person", "Zhang San")
	if err != nil || from == nil {
		t.Fatalf("GetEntity from failed: %v", err)
	}
	to, err := mem.GetEntity(ctx, "org", "Acme Corp")
	if err != nil || to == nil {
		t.Fatalf("GetEntity to failed: %v", err)
	}
	return from.DBID, to.DBID
}

func TestUpsertRelation_MinorChangeUpdatesInPlace(t *testing.T) {
	store := testutil.NewMockStore()
	mem := NewSemanticMem(store, &testutil.MockIntentSubmitter{})
	ctx := context.Background()
	fromID, toID := setupRelationFixture(t, mem, ctx)

	rel := types.Relation{RelationType: "WORKS_AT", FromDBID: fromID, ToDBID: toID, Weight: 0.5}
	if err := mem.UpsertRelation(ctx, rel, types.TaintNone); err != nil {
		t.Fatalf("initial UpsertRelation failed: %v", err)
	}
	first, err := mem.queryActiveRelation(ctx, fromID, toID, "WORKS_AT")
	if err != nil || first == nil {
		t.Fatalf("queryActiveRelation after initial insert failed: %v", err)
	}

	rel.Weight = 0.55 // delta 0.05 < 阈值 0.2 —— 判定为证据累积，非实质变化
	if err := mem.UpsertRelation(ctx, rel, types.TaintNone); err != nil {
		t.Fatalf("minor-change UpsertRelation failed: %v", err)
	}

	second, err := mem.queryActiveRelation(ctx, fromID, toID, "WORKS_AT")
	if err != nil || second == nil {
		t.Fatalf("queryActiveRelation after minor update failed: %v", err)
	}
	if second.DBID != first.DBID {
		t.Errorf("minor change should update in place, got new DBID %d (was %d)", second.DBID, first.DBID)
	}
	if second.Weight != 0.55 {
		t.Errorf("expected weight 0.55 after MAX merge, got %v", second.Weight)
	}

	var count int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM semantic_relations WHERE source_id=? AND target_id=? AND relation_type='WORKS_AT'`,
		fromID, toID).Scan(&count); err != nil {
		t.Fatalf("count query failed: %v", err)
	}
	if count != 1 {
		t.Errorf("minor change must not create a new version row, got %d rows", count)
	}
}

func TestUpsertRelation_SubstantialChangeCreatesNewVersion(t *testing.T) {
	store := testutil.NewMockStore()
	mem := NewSemanticMem(store, &testutil.MockIntentSubmitter{})
	ctx := context.Background()
	fromID, toID := setupRelationFixture(t, mem, ctx)

	rel := types.Relation{RelationType: "WORKS_AT", FromDBID: fromID, ToDBID: toID, Weight: 0.5}
	if err := mem.UpsertRelation(ctx, rel, types.TaintNone); err != nil {
		t.Fatalf("initial UpsertRelation failed: %v", err)
	}
	first, err := mem.queryActiveRelation(ctx, fromID, toID, "WORKS_AT")
	if err != nil || first == nil {
		t.Fatalf("queryActiveRelation after initial insert failed: %v", err)
	}

	rel.Weight = 0.95 // delta 0.45 > 阈值 0.2 —— 实质变化，触发信念修正
	if err := mem.UpsertRelation(ctx, rel, types.TaintNone); err != nil {
		t.Fatalf("substantial-change UpsertRelation failed: %v", err)
	}

	second, err := mem.queryActiveRelation(ctx, fromID, toID, "WORKS_AT")
	if err != nil || second == nil {
		t.Fatalf("queryActiveRelation after revision failed: %v", err)
	}
	if second.DBID == first.DBID {
		t.Fatalf("substantial change should create a new version row, DBID unchanged (%d)", second.DBID)
	}
	if second.Weight != 0.95 {
		t.Errorf("expected new active weight 0.95, got %v", second.Weight)
	}

	var status string
	var supersededBy int64
	var validUntil sql.NullInt64
	if err := store.DB().QueryRowContext(ctx,
		`SELECT status, superseded_by, valid_until FROM semantic_relations WHERE id = ?`, first.DBID,
	).Scan(&status, &supersededBy, &validUntil); err != nil {
		t.Fatalf("query old row failed: %v", err)
	}
	if status != "superseded" {
		t.Errorf("old row status = %q, want superseded", status)
	}
	if supersededBy != second.DBID {
		t.Errorf("old row superseded_by = %d, want %d", supersededBy, second.DBID)
	}
	if !validUntil.Valid || validUntil.Int64 <= 0 {
		t.Errorf("old row valid_until should be set, got %+v", validUntil)
	}

	var activeCount int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM semantic_relations WHERE source_id=? AND target_id=? AND relation_type='WORKS_AT' AND status='active'`,
		fromID, toID).Scan(&activeCount); err != nil {
		t.Fatalf("active count query failed: %v", err)
	}
	if activeCount != 1 {
		t.Errorf("exactly one active version expected, got %d", activeCount)
	}
}

func TestTemporalExpirer_ExpiresStaleRelations(t *testing.T) {
	store := testutil.NewMockStore()
	mem := NewSemanticMem(store, &testutil.MockIntentSubmitter{})
	ctx := context.Background()
	fromID, toID := setupRelationFixture(t, mem, ctx)

	rel := types.Relation{RelationType: "TEMP_ASSIGNED_TO", FromDBID: fromID, ToDBID: toID, Weight: 1.0}
	if err := mem.UpsertRelation(ctx, rel, types.TaintNone); err != nil {
		t.Fatalf("UpsertRelation failed: %v", err)
	}
	// 直接把 valid_until 设为过去（模拟"临时指派已到期"）。
	if _, err := store.DB().ExecContext(ctx,
		`UPDATE semantic_relations SET valid_until = 1 WHERE source_id=? AND target_id=? AND relation_type='TEMP_ASSIGNED_TO'`,
		fromID, toID); err != nil {
		t.Fatalf("failed to set valid_until: %v", err)
	}

	expirer := graph.NewTemporalExpirer(store.DB())
	affected, err := expirer.ExpireStale(ctx)
	if err != nil {
		t.Fatalf("ExpireStale failed: %v", err)
	}
	if affected < 1 {
		t.Errorf("expected at least 1 expired row, got %d", affected)
	}

	var status string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT status FROM semantic_relations WHERE source_id=? AND target_id=? AND relation_type='TEMP_ASSIGNED_TO'`,
		fromID, toID).Scan(&status); err != nil {
		t.Fatalf("query status failed: %v", err)
	}
	if status != "expired" {
		t.Errorf("relation status = %q, want expired", status)
	}
}
