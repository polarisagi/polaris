package consolidation

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/memory/retrieval"
	memstore "github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/memory/testutil"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// mockSummarizer 是 memory.LLMSummarizer 的测试替身，直接包一层 testutil.MockProvider
// 的固定响应/失败开关，用于验证 ConsolidationPipeline 的 LLM 主路径与规则 fallback。
type mockSummarizer struct {
	resp string
	fail bool
}

func (m *mockSummarizer) Summarize(ctx context.Context, text string, maxTokens int) (string, error) {
	return m.InferRaw(ctx, text, maxTokens)
}

func (m *mockSummarizer) InferRaw(ctx context.Context, prompt string, maxTokens int) (string, error) {
	if m.fail {
		return "", apperr.New(apperr.CodeInternal, "mock summarizer failure")
	}
	return m.resp, nil
}

func TestConsolidationPipeline(t *testing.T) {
	ctx := context.Background()
	store := testutil.NewMockStore()
	semantic := memstore.NewSemanticMem(store, &testutil.MockIntentSubmitter{})
	episodic := memstore.NewEpisodicMem(store)
	skills := &mockSkillRegistry{}
	summarizer := &mockSummarizer{resp: `{"entities":[{"name":"abc","type":"concept"}],"relations":[]}`}

	pipe := NewConsolidationPipelineFull(episodic, semantic, skills, summarizer, nil, nil, nil)

	// Test Run on empty
	err := pipe.Run(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}

	// Add events to episodic
	for i := 0; i < 15; i++ {
		_ = episodic.Append(ctx, types.Event{
			ID:        "e1",
			TaskID:    "s1",
			Type:      "tool_call",
			Payload:   []byte(`{"tool_name": "test_tool", "success": true}`),
			CreatedAt: time.Now(),
		}, types.TaintNone)
	}

	err = pipe.Run(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}

	// Test MarkColdEpisodicEvents
	err = pipe.MarkColdEpisodicEvents(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}

	// Test ruleExtract fallback
	summarizer.fail = true
	pipe2 := NewConsolidationPipelineFull(episodic, semantic, skills, summarizer, nil, nil, nil)
	err = pipe2.Run(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}

	// Test Jaccard
	j := retrieval.JaccardSimilarity("prog_lang", "language")
	if j < 0 {
		t.Fatal("jaccard failed")
	}

	// Test ForgettingManager
	fm := NewForgettingManager(store, nil, 0.01)

	_ = store.Put(ctx, []byte("events:e100"), []byte(`{"id":"e100","topic":"memory","salience":0.01,"occurred_at":0}`))
	err = fm.PeriodicCleanup()
	if err != nil {
		t.Fatal(err)
	}

	// Test decay and exp
	decay := fm.UpdateDecay(1.0, 100)
	if decay < 0 {
		t.Fatal("decay failed")
	}

	ca := NewColdArchiver(store)
	err = ca.PhysicalCompact()
	if err != nil {
		t.Fatal(err)
	}
}

type mockSkillRegistry struct{}

func (m *mockSkillRegistry) Register(ctx context.Context, meta types.SkillMeta) error { return nil }
func (m *mockSkillRegistry) List(ctx context.Context, filter types.SkillFilter) ([]types.SkillMeta, error) {
	return nil, nil
}
func (m *mockSkillRegistry) Get(ctx context.Context, name string, version string) (*types.SkillMeta, error) {
	return nil, nil
}
func (m *mockSkillRegistry) Delete(ctx context.Context, name string) error { return nil }
func (m *mockSkillRegistry) Deprecate(ctx context.Context, name string, alternative string, reason string) error {
	return nil
}

type mockCognitiveForgetting struct {
	deletedFTS []string
	deletedVec []string
}

func (m *mockCognitiveForgetting) FTSIndex(docID, text string) error { return nil }

func (m *mockCognitiveForgetting) FTSDelete(docID string) error {
	m.deletedFTS = append(m.deletedFTS, docID)
	return nil
}

func (m *mockCognitiveForgetting) GraphRelate(fromID, edgeType, toID string, weight float64) error {
	return nil
}

func (m *mockCognitiveForgetting) VecUpsert(id string, embedding []float32) error { return nil }
func (m *mockCognitiveForgetting) VecDelete(id string) error {
	m.deletedVec = append(m.deletedVec, id)
	return nil
}
func (m *mockCognitiveForgetting) VecKNN(query []float32, k int) ([]types.CognitiveSearchResult, error) {
	return nil, nil
}
func (m *mockCognitiveForgetting) FTSSearch(query string, k int) ([]types.CognitiveSearchResult, error) {
	return nil, nil
}

func TestForgettingManager_DecayAndArchive(t *testing.T) {
	mockStore := testutil.NewMockStore()
	mockCogn := &mockCognitiveForgetting{}

	fm := NewForgettingManager(mockStore, mockCogn, 0.01)

	// test UpdateDecay value
	// ageHours = 24 * 35 = 840
	// salience = 0.5
	// decay = 0.5 * exp(-0.01 * 840 / 24) = 0.5 * exp(-0.35)
	decay := fm.UpdateDecay(0.5, 24*35)
	expected := 0.5 * math.Exp(-0.35)
	if math.Abs(decay-expected) > 1e-9 {
		t.Fatalf("decay mismatch: got %v, expected %v", decay, expected)
	}
}
