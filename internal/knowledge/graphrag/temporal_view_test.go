package graphrag

import (
	"testing"
	"time"
)

func TestAsOfFilter_ZeroValueIsActiveFastPath(t *testing.T) {
	var f AsOfFilter
	if !f.IsZero() {
		t.Fatal("zero-value AsOfFilter should report IsZero() == true")
	}
	where, args := f.SQLWhere("r")
	if where != "r.status = 'active'" {
		t.Errorf("zero filter WHERE = %q, want \"r.status = 'active'\"", where)
	}
	if len(args) != 0 {
		t.Errorf("zero filter should produce no args, got %v", args)
	}
}

func TestAsOfFilter_ZeroValueNoAlias(t *testing.T) {
	var f AsOfFilter
	where, _ := f.SQLWhere("")
	if where != "status = 'active'" {
		t.Errorf("empty-alias zero filter WHERE = %q, want \"status = 'active'\"", where)
	}
}

func TestAsOfFilter_NonZeroProducesTimeWindow(t *testing.T) {
	at := time.UnixMilli(1_700_000_000_000)
	f := AsOfFilter{At: at}
	if f.IsZero() {
		t.Fatal("non-zero AsOfFilter should report IsZero() == false")
	}
	where, args := f.SQLWhere("r")
	wantWhere := "(r.valid_from IS NULL OR r.valid_from <= ?) AND (r.valid_until IS NULL OR r.valid_until > ?)"
	if where != wantWhere {
		t.Errorf("WHERE = %q, want %q", where, wantWhere)
	}
	if len(args) != 2 || args[0] != at.UnixMilli() || args[1] != at.UnixMilli() {
		t.Errorf("args = %v, want [%d %d]", args, at.UnixMilli(), at.UnixMilli())
	}
}

// TestFetchNeighbors_AsOfReplaysHistoricalEdge 端到端验证 ADR-0083：一条关系边
// 被信念修正（旧边 superseded）后，默认视图（零值 AsOf）只看到新目标，AsOf 回放到
// 修正之前的时点则看到旧目标——证明 GraphTraverser 的 AsOf 接入生效。
func TestFetchNeighbors_AsOfReplaysHistoricalEdge(t *testing.T) {
	db := setupTraverserTestDB(t)
	defer db.Close()
	gt := &GraphTraverser{db: db}

	// setupTraverserTestDB 已插入实体 1=GraphRAG(Concept), 2=BM25(Tool)，
	// 及关系 1--USES-->2。补一个第三实体作为"信念修正后的新目标"。
	if _, err := db.Exec(`INSERT INTO semantic_entities(entity_type, name, status, created_at, updated_at)
		VALUES ('Tool','Vector-Search','active',0,0)`); err != nil {
		t.Fatalf("insert third entity failed: %v", err)
	}

	t1 := int64(1000)
	t2 := int64(2000)

	// 把既有的 1--USES-->2 边改造成"T1 时刻有效，T2 时刻被取代"的历史版本。
	if _, err := db.Exec(`UPDATE semantic_relations SET status='superseded', valid_from=?, valid_until=?, superseded_by=3
		WHERE source_id=1 AND target_id=2 AND relation_type='USES'`, t1, t2); err != nil {
		t.Fatalf("supersede old relation failed: %v", err)
	}
	// 新的活跃边：1--USES-->3（entity id 3 = Vector-Search），T2 时刻起生效。
	if _, err := db.Exec(`INSERT INTO semantic_relations
		(id, source_id, target_id, relation_type, weight, created_at, status, valid_from)
		VALUES (99, 1, 3, 'USES', 1.0, ?, 'active', ?)`, t2, t2); err != nil {
		t.Fatalf("insert new relation failed: %v", err)
	}

	ctx := t.Context()

	// 零值 AsOf（当前视图）：应只看到新目标 3，不应看到已被取代的 2。
	current, err := gt.fetchNeighbors(ctx, 1, 10, TraverseOptions{})
	if err != nil {
		t.Fatalf("fetchNeighbors (current) failed: %v", err)
	}
	assertContainsOnly(t, current, 3)

	// AsOf(T1+500ms)：回放到旧边生效期间，应看到旧目标 2，看不到尚未创建的 3。
	past, err := gt.fetchNeighbors(ctx, 1, 10, TraverseOptions{AsOf: AsOfFilter{At: time.UnixMilli(t1 + 500)}})
	if err != nil {
		t.Fatalf("fetchNeighbors (AsOf past) failed: %v", err)
	}
	assertContainsOnly(t, past, 2)
}

func assertContainsOnly(t *testing.T, got []int64, want int64) {
	t.Helper()
	if len(got) != 1 || got[0] != want {
		t.Errorf("got %v, want exactly [%d]", got, want)
	}
}
