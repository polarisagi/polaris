package retrieval_test

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/memory/retrieval"
	"github.com/polarisagi/polaris/internal/store"
)

func setupCascadeTestDB(t *testing.T) *sql.DB {
	t.Helper()
	schemaFS := os.DirFS("../../protocol/schema").(fs.ReadDirFS)
	st, err := store.OpenSQLite(":memory:", schemaFS)
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.DB().Close() })
	return st.DB()
}

func insertEntity(t *testing.T, db *sql.DB, id int64, name string) {
	t.Helper()
	now := time.Now().UnixMilli()
	_, err := db.Exec(
		`INSERT INTO semantic_entities(id, entity_type, name, status, created_at, updated_at) VALUES (?, 'Concept', ?, 'active', ?, ?)`,
		id, name, now, now,
	)
	if err != nil {
		t.Fatalf("failed to insert entity %d: %v", id, err)
	}
}

func insertRelation(t *testing.T, db *sql.DB, sourceID, targetID int64, relType string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO semantic_relations(source_id, target_id, relation_type, created_at) VALUES (?, ?, ?, ?)`,
		sourceID, targetID, relType, time.Now().UnixMilli(),
	)
	if err != nil {
		t.Fatalf("failed to insert relation %d->%d: %v", sourceID, targetID, err)
	}
}

func entityStatus(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	var status string
	if err := db.QueryRow(`SELECT status FROM semantic_entities WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatalf("failed to query status for %d: %v", id, err)
	}
	return status
}

// TestCascadeInvalidator_TwoHopChain 验证 GD-14-001 递归 CTE 重写后语义与原 BFS
// 实现一致：1(seed) -[derived_from]-> 2 -[depends_on]-> 3 -[relates_to]-> 4，
// maxCascadeHops=2 时 2、3 应被标记 pending_review，4（第 3 跳）不应被触达。
func TestCascadeInvalidator_TwoHopChain(t *testing.T) {
	db := setupCascadeTestDB(t)
	ctx := context.Background()

	insertEntity(t, db, 1, "seed")
	insertEntity(t, db, 2, "hop1")
	insertEntity(t, db, 3, "hop2")
	insertEntity(t, db, 4, "hop3")
	insertRelation(t, db, 1, 2, "derived_from")
	insertRelation(t, db, 2, 3, "depends_on")
	insertRelation(t, db, 3, 4, "relates_to")

	ci := retrieval.NewCascadeInvalidator(db)
	affected, err := ci.Invalidate(ctx, 1)
	if err != nil {
		t.Fatalf("Invalidate failed: %v", err)
	}

	affectedSet := map[int64]bool{}
	for _, id := range affected {
		affectedSet[id] = true
	}
	if !affectedSet[2] || !affectedSet[3] {
		t.Errorf("expected entities 2 and 3 to be cascaded, got %v", affected)
	}
	if affectedSet[4] {
		t.Errorf("expected entity 4 (3 hops away) NOT to be cascaded, got %v", affected)
	}
	if affectedSet[1] {
		t.Errorf("expected seed entity 1 not to appear in its own cascade result")
	}

	if got := entityStatus(t, db, 2); got != "pending_review" {
		t.Errorf("entity 2: expected status pending_review, got %q", got)
	}
	if got := entityStatus(t, db, 3); got != "pending_review" {
		t.Errorf("entity 3: expected status pending_review, got %q", got)
	}
	if got := entityStatus(t, db, 4); got != "active" {
		t.Errorf("entity 4: expected status to remain active, got %q", got)
	}
}

// TestCascadeInvalidator_NoRelations 验证孤立实体（无出边/入边）级联结果为空，
// 不产生任何 pending_review 标记，也不报错。
func TestCascadeInvalidator_NoRelations(t *testing.T) {
	db := setupCascadeTestDB(t)
	ctx := context.Background()

	insertEntity(t, db, 1, "isolated")

	ci := retrieval.NewCascadeInvalidator(db)
	affected, err := ci.Invalidate(ctx, 1)
	if err != nil {
		t.Fatalf("Invalidate failed: %v", err)
	}
	if len(affected) != 0 {
		t.Errorf("expected no cascaded entities, got %v", affected)
	}
}

// TestCascadeInvalidator_CycleDoesNotHang 验证有环图（1<->2<->3<->1）不会导致递归
// CTE 无限展开——hop 列的终止条件必须严格生效，测试本身能在正常超时内返回即为通过。
func TestCascadeInvalidator_CycleDoesNotHang(t *testing.T) {
	db := setupCascadeTestDB(t)
	ctx := context.Background()

	insertEntity(t, db, 1, "a")
	insertEntity(t, db, 2, "b")
	insertEntity(t, db, 3, "c")
	insertRelation(t, db, 1, 2, "relates_to")
	insertRelation(t, db, 2, 3, "relates_to")
	insertRelation(t, db, 3, 1, "relates_to")

	ci := retrieval.NewCascadeInvalidator(db)
	affected, err := ci.Invalidate(ctx, 1)
	if err != nil {
		t.Fatalf("Invalidate failed on cyclic graph: %v", err)
	}
	affectedSet := map[int64]bool{}
	for _, id := range affected {
		affectedSet[id] = true
	}
	if !affectedSet[2] || !affectedSet[3] {
		t.Errorf("expected both neighbors reachable within 2 hops on a 3-cycle, got %v", affected)
	}
	if affectedSet[1] {
		t.Errorf("seed entity should not appear in its own cascade result even via cycle, got %v", affected)
	}
}

// TestCascadeInvalidator_OnlyActiveEntitiesMarked 验证 markPendingReview 只更新
// status='active' 的实体，已 superseded 的邻居不会被误改。
func TestCascadeInvalidator_OnlyActiveEntitiesMarked(t *testing.T) {
	db := setupCascadeTestDB(t)
	ctx := context.Background()

	insertEntity(t, db, 1, "seed")
	insertEntity(t, db, 2, "already-superseded")
	insertRelation(t, db, 1, 2, "derived_from")

	now := time.Now().UnixMilli()
	if _, err := db.Exec(`UPDATE semantic_entities SET status='superseded', updated_at=? WHERE id=2`, now); err != nil {
		t.Fatalf("failed to pre-set status: %v", err)
	}

	ci := retrieval.NewCascadeInvalidator(db)
	if _, err := ci.Invalidate(ctx, 1); err != nil {
		t.Fatalf("Invalidate failed: %v", err)
	}
	if got := entityStatus(t, db, 2); got != "superseded" {
		t.Errorf("expected already-superseded entity to remain superseded, got %q", got)
	}
}
