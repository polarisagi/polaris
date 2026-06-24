package store

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/memory/testutil"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestEpisodicMem(t *testing.T) {
	store := testutil.NewMockStore()
	mem := NewEpisodicMemWithGraph(store, nil)

	ctx := context.Background()

	// Test Append
	ev := types.Event{
		ID:        "evt1",
		Type:      types.EventIntent,
		Payload:   []byte(`{"key":"value"}`),
		CreatedAt: time.Now(),
	}
	err := mem.Append(ctx, ev, types.TaintNone)
	if err != nil {
		t.Fatalf("Append failed: %v", err)
	}

	// Test Query
	q := types.EpisodicQuery{
		K:             10,
		MaxTaintLevel: types.TaintHigh,
	}
	events, err := mem.Query(ctx, q)
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	if len(events) != 1 || events[0].Event.(*types.Event).ID != "evt1" {
		t.Errorf("Query returned unexpected events: %+v", events)
	}

	// Test Consolidate
	// First we need a semantic memory object
	semanticMem := NewSemanticMem(store, &testutil.MockIntentSubmitter{})
	err = mem.Consolidate(ctx, semanticMem)
	if err != nil {
		t.Fatalf("Consolidate failed: %v", err)
	}

	// Test MarkCold
	_, err = mem.MarkCold(ctx, "evt1", time.Now())
	if err != nil {
		t.Fatalf("MarkCold failed: %v", err)
	}

	// Test loadEventsFromStore
	evs, err := mem.loadEventsFromStore(ctx)
	if err != nil {
		t.Fatalf("loadEventsFromStore failed: %v", err)
	}
	if len(evs) != 1 {
		t.Errorf("Expected 1 event from store, got %d", len(evs))
	}
}

func TestSemanticMem(t *testing.T) {
	store := testutil.NewMockStore()
	mem := NewSemanticMem(store, &testutil.MockIntentSubmitter{})
	ctx := context.Background()

	// Test StoreDocument
	doc := types.Document{
		ID:        "doc1",
		SourceURI: "test doc",
	}
	err := mem.StoreDocument(ctx, doc, types.TaintNone)
	if err != nil {
		t.Fatalf("StoreDocument failed: %v", err)
	}

	// Test GetDocument
	d, err := mem.GetDocument(ctx, "doc1")
	if err != nil || d.SourceURI != "test doc" {
		t.Fatalf("GetDocument failed: %v", err)
	}

	// Test Archive
	err = mem.Archive(ctx, "doc1", "obsolete")
	if err != nil {
		t.Fatalf("Archive failed: %v", err)
	}

	// Test UpsertFact — 现在直接写 DB（不再依赖 MutationBus）
	fact := types.Entity{
		ID:   "ent1",
		Name: "Alice",
		Type: "person",
	}
	err = mem.UpsertFact(ctx, fact, types.TaintNone)
	if err != nil {
		t.Fatalf("UpsertFact failed: %v", err)
	}

	// 写入第二个实体供关系测试使用
	fact2 := types.Entity{
		ID:   "ent2",
		Name: "Bob",
		Type: "person",
	}
	if err2 := mem.UpsertFact(ctx, fact2, types.TaintNone); err2 != nil {
		t.Fatalf("UpsertFact Bob failed: %v", err2)
	}

	// 查回 DBID 供 UpsertRelation 使用（UpsertRelation 需要 DB 整数主键，而非 ephemeral string ID）
	alice, err := mem.GetEntity(ctx, "person", "Alice")
	if err != nil || alice == nil {
		t.Fatalf("GetEntity Alice after UpsertFact failed: %v", err)
	}
	bob, err := mem.GetEntity(ctx, "person", "Bob")
	if err != nil || bob == nil {
		t.Fatalf("GetEntity Bob after UpsertFact failed: %v", err)
	}

	// Test UpsertRelation
	rel := types.Relation{
		FromEntityID: "ent1",
		ToEntityID:   "ent2",
		RelationType: "is_friend_with",
		FromDBID:     alice.DBID,
		ToDBID:       bob.DBID,
	}
	err = mem.UpsertRelation(ctx, rel, types.TaintNone)
	if err != nil {
		t.Fatalf("UpsertRelation failed: %v", err)
	}

	// Test GetEntity — UpsertFact 已实际写 DB，直接查询无需手动 INSERT
	ent, err := mem.GetEntity(ctx, "person", "Alice")
	if err != nil {
		t.Fatalf("GetEntity failed: %v", err)
	}
	if ent == nil || ent.Name != "Alice" {
		t.Errorf("Unexpected GetEntity result")
	}

	// Test ListActiveEntities
	_, err = mem.ListActiveEntities(ctx, "person", 10)
	if err != nil {
		t.Fatalf("ListActiveEntities failed: %v", err)
	}

	// Test MarkEntitySuperseded
	err = mem.MarkEntitySuperseded(ctx, 1, 2)
	if err != nil {
		t.Fatalf("MarkEntitySuperseded failed: %v", err)
	}

	// Test UpsertUserProfile
	prof := types.UserProfile{
		ProfileKey:  "user1",
		StableFacts: map[string]any{"name": "Alice"},
	}
	err = mem.UpsertUserProfile(ctx, prof)
	if err != nil {
		t.Fatalf("UpsertUserProfile failed: %v", err)
	}

	// Test GetUserProfile
	profRet, err := mem.GetUserProfile(ctx, "user1")
	if err != nil {
		t.Fatalf("GetUserProfile failed: %v", err)
	}
	if profRet != nil && profRet.ProfileKey != "user1" {
		t.Errorf("Unexpected GetUserProfile result")
	}

	// Test StoreStats
	_, _ = mem.StoreStats()
}
