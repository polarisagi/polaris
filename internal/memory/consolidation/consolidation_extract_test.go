package consolidation

import (
	"context"
	"testing"
	"time"

	memstore "github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/memory/testutil"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// fakeSharedExtractor 是 SharedEntityExtractor 的测试替身（D3/ADR-0077），
// 记录调用次数以验证：entityExtractor 非 nil 时它是唯一被调用的抽取路径，
// llmExtract/ruleExtract 不应再被触发；entityExtractor 返回 err 时才允许
// 降级到 ruleExtract。
type fakeSharedExtractor struct {
	calls     int
	entities  []*types.Entity
	relations []*types.Relation
	err       error
}

func (f *fakeSharedExtractor) ExtractEntitiesAndRelations(
	_ context.Context, _, _ string,
) ([]*types.Entity, []*types.Relation, error) {
	f.calls++
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.entities, f.relations, nil
}

func scoredEventsWithPayload(sessionID, payload string, n int) []types.ScoredEvent {
	out := make([]types.ScoredEvent, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, types.ScoredEvent{
			Event: &types.Event{
				ID:        "e1",
				TaskID:    sessionID,
				Type:      "tool_call",
				Payload:   []byte(payload),
				CreatedAt: time.Now(),
			},
		})
	}
	return out
}

// TestConsolidationPipeline_EntityExtractor_TakesPriorityOverLLM 验证
// extractEntitiesAndRelations 在 entityExtractor 非 nil 时优先、且唯一地走该
// 路径——llmExtract 分支（依赖 p.summarizer）完全不可达。summarizer 的响应中
// 刻意埋入一个只有 llmExtract 才会产出的实体名，用来反证它没有被使用：两条路径
// 在 consolidation_extract.go 中是互斥的 if/else 分支，不存在两者都写入的可能。
func TestConsolidationPipeline_EntityExtractor_TakesPriorityOverLLM(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewMockStore()
	semantic := memstore.NewSemanticMem(store, &testutil.MockIntentSubmitter{})
	episodic := memstore.NewEpisodicMem(store)
	skills := &mockSkillRegistry{}

	summarizer := &mockSummarizer{
		resp: `{"entities":[{"name":"should-not-be-used","type":"concept"}],"relations":[]}`,
	}
	extractor := &fakeSharedExtractor{
		entities: []*types.Entity{
			{ID: "shared_ent_1", Name: "shared-entity", Type: "concept", Confidence: 0.9},
		},
		relations: []*types.Relation{},
	}

	pipe := NewConsolidationPipelineFull(episodic, semantic, skills, summarizer, nil, nil, nil)
	pipe.WithEntityExtractor(extractor)

	sessionID := "d3-session-1"
	events := scoredEventsWithPayload(sessionID, `{"tool_name": "test_tool", "success": true}`, 15)

	entities, _, err := pipe.extractEntitiesAndRelations(ctx, sessionID, events)
	if err != nil {
		t.Fatalf("extractEntitiesAndRelations failed: %v", err)
	}

	if extractor.calls != 1 {
		t.Fatalf("expected SharedEntityExtractor to be called exactly once, got %d", extractor.calls)
	}
	if len(entities) != 1 || entities[0].Name != "shared-entity" {
		t.Fatalf("expected exactly the SharedEntityExtractor's entity to be returned, got %+v", entities)
	}
	for _, e := range entities {
		if e.Name == "should-not-be-used" {
			t.Fatal("llmExtract's entity must not appear when entityExtractor takes priority")
		}
	}
}

// TestConsolidationPipeline_EntityExtractor_FallsBackToRuleOnError 验证
// entityExtractor 返回 err 时降级到 ruleExtract（正则回退），而不是转而调用
// llmExtract——避免"降级路径"意外变成第二条 LLM 调用路径，与 D3 消除重复
// LLM 燃烧的目标相悖。
func TestConsolidationPipeline_EntityExtractor_FallsBackToRuleOnError(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewMockStore()
	semantic := memstore.NewSemanticMem(store, &testutil.MockIntentSubmitter{})
	episodic := memstore.NewEpisodicMem(store)
	skills := &mockSkillRegistry{}

	summarizer := &mockSummarizer{
		resp: `{"entities":[{"name":"should-not-be-used-either","type":"concept"}],"relations":[]}`,
	}
	extractor := &fakeSharedExtractor{err: apperr.New(apperr.CodeInternal, "simulated graphrag extraction failure")}

	pipe := NewConsolidationPipelineFull(episodic, semantic, skills, summarizer, nil, nil, nil)
	pipe.WithEntityExtractor(extractor)

	sessionID := "d3-session-2"
	// URL 模式是 ruleExtract 唯一能确定性命中的规则（getEntityExtractPatterns），
	// 用其存在与否来判定真正走的是 ruleExtract 而非 llmExtract。
	events := scoredEventsWithPayload(sessionID, "see https://example.com/docs for details", 15)

	entities, _, err := pipe.extractEntitiesAndRelations(ctx, sessionID, events)
	if err != nil {
		t.Fatalf("extractEntitiesAndRelations failed: %v", err)
	}

	if extractor.calls != 1 {
		t.Fatalf("expected SharedEntityExtractor to be attempted exactly once, got %d", extractor.calls)
	}

	foundURL := false
	for _, e := range entities {
		if e.Name == "should-not-be-used-either" {
			t.Fatal("expected fallback to ruleExtract on entityExtractor error, not llmExtract")
		}
		if e.Type == "url" {
			foundURL = true
		}
	}
	if !foundURL {
		t.Fatalf("expected ruleExtract to extract the URL entity, got %+v", entities)
	}
}
