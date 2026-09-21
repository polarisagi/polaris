package retrieval

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/memory/testutil"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/types"
)

// 以下 TestProjectIsolation* 用例由 tools/memory_isolation_check.go 纳入 make lint（ADR-0097 决策三修订）。
// 负向自测（tools/lint-selftest.txt）拆掉 retriever.Search 里的 projectScopedSource 包装，
// 这些用例必须转红。

const alphaMarker = "ALPHA-MARKER-7f3c"

// seedEvents 写入：项目 A 的标记事件、项目 B 的事件、未打标（默认项目）事件，
// 以及 A/B 各一个持续簇。
func seedEvents(t *testing.T) *testutil.MockStore {
	t.Helper()
	st := testutil.NewMockStore()
	ctx := context.Background()
	put := func(key string, v any) {
		b, _ := json.Marshal(v)
		if err := st.Put(ctx, []byte(key), b); err != nil {
			t.Fatal(err)
		}
	}
	put("episodic:evA", types.Event{ID: "evA", TaskID: "alphatask", ProjectID: "prj_a", Payload: []byte(alphaMarker + " 部署密钥放在 vault")})
	put("episodic:evB", types.Event{ID: "evB", TaskID: "bravotask", ProjectID: "prj_b", Payload: []byte("BRAVO 部署流程")})
	put("episodic:evD", types.Event{ID: "evD", TaskID: "sD", Payload: []byte("DEFAULT 部署笔记")})
	put("durative_group:gA", map[string]any{"id": "gA", "summary": alphaMarker, "status": "active", "project_id": "prj_a"})
	put("durative_group:gB", map[string]any{"id": "gB", "summary": "bravo", "status": "active", "project_id": "prj_b"})
	return st
}

// fakeInnerSource 在四个方法里各返回一整套各类来源的片段，用来验证包装层对每一路都生效。
type fakeInnerSource struct{}

func allKinds() []types.ScoredFragment {
	srcs := []string{
		"episodic:evA", "episodic:evB", "episodic:evD", // 情景事件
		"evA",                                    // 图路径以事件 ID 直接作 Source
		"durative_group:gA", "durative_group:gB", // 持续簇
		"entity:12", "sement_Tool_go", "reflection:r1", "chunk:c1", // 全局层
		"episodic_row:9",  // 未回填 uuid 的历史投影行
		"episodic:evGone", // 索引命中但原文已不在 KV
	}
	out := make([]types.ScoredFragment, 0, len(srcs))
	for _, s := range srcs {
		out = append(out, types.ScoredFragment{Source: s, Content: s})
	}
	return out
}

func (fakeInnerSource) SearchBM25(context.Context, string, int) ([]types.ScoredFragment, error) {
	return allKinds(), nil
}
func (fakeInnerSource) SearchVector(context.Context, []float32, int) ([]types.ScoredFragment, error) {
	return allKinds(), nil
}
func (fakeInnerSource) SearchGraph(context.Context, string, int) ([]types.ScoredFragment, error) {
	return allKinds(), nil
}
func (fakeInnerSource) SearchExtraPaths(context.Context, string, []float32, int) ([]search.ExtraPath, error) {
	return []search.ExtraPath{{Results: allKinds()}, {Results: allKinds()}}, nil
}

func sources(frags []types.ScoredFragment) map[string]bool {
	m := map[string]bool{}
	for _, f := range frags {
		m[f.Source] = true
	}
	return m
}

// TestProjectIsolation_WrapperFiltersEverySourceKind 包装层对四个方法的全部产出按来源类型过滤。
func TestProjectIsolation_WrapperFiltersEverySourceKind(t *testing.T) {
	ctx := context.Background()
	st := seedEvents(t)
	ds := newProjectScopedSource(fakeInnerSource{}, st, "prj_b")

	want := map[string]bool{
		"episodic:evB": true, "durative_group:gB": true,
		"entity:12": true, "sement_Tool_go": true, "reflection:r1": true, "chunk:c1": true,
	}
	check := func(path string, frags []types.ScoredFragment) {
		got := sources(frags)
		for s := range got {
			if !want[s] {
				t.Errorf("%s: 项目 B 不应看到 %s", path, s)
			}
		}
		for s := range want {
			if !got[s] {
				t.Errorf("%s: 项目 B 应保留 %s", path, s)
			}
		}
	}
	r, _ := ds.SearchBM25(ctx, "", 10)
	check("bm25", r)
	r, _ = ds.SearchVector(ctx, nil, 10)
	check("vector", r)
	r, _ = ds.SearchGraph(ctx, "", 10)
	check("graph", r)
	extra, _ := ds.SearchExtraPaths(ctx, "", nil, 10)
	for i, p := range extra {
		check("extra#"+string(rune('0'+i)), p.Results)
	}

	// 默认项目：只见未打标事件与无法反查的历史行；看不到任何具名项目的内容。
	def := sources(func() []types.ScoredFragment {
		r, _ := newProjectScopedSource(fakeInnerSource{}, st, types.DefaultProjectID).SearchBM25(ctx, "", 10)
		return r
	}())
	if !def["episodic:evD"] || !def["episodic_row:9"] || def["episodic:evA"] || def["episodic:evB"] || def["durative_group:gA"] {
		t.Errorf("默认项目作用域错误: %v", def)
	}

	// 不限项目（后台检索）：不包装。
	if _, wrapped := newProjectScopedSource(fakeInnerSource{}, st, "").(*projectScopedSource); wrapped {
		t.Error("projectID 为空时不应包装")
	}
}

// TestProjectIsolation_HybridSearchTier0 端到端：Tier0（KV 前缀扫描 BM25 + Simhash + 图路径）。
func TestProjectIsolation_HybridSearchTier0(t *testing.T) {
	st := seedEvents(t)
	hr := NewHybridRetrieverFull(st, &fakeGraph{nodes: []types.ScoredNode{{ID: "evA", Score: 1}}}, nil, nil)
	assertNoMarker(t, hr, "prj_b")
	assertHasMarker(t, hr, "prj_a")
}

// fakeCognitive Tier1：FTS 与向量都命中项目 A 的事件。
type fakeCognitive struct{ protocol.CognitiveSearcher }

func (fakeCognitive) FTSSearch(string, int) ([]types.CognitiveSearchResult, error) {
	return []types.CognitiveSearchResult{{ID: "evA", Score: 9}, {ID: "evB", Score: 1}}, nil
}
func (fakeCognitive) VecKNN([]float32, int) ([]types.CognitiveSearchResult, error) {
	return []types.CognitiveSearchResult{{ID: "evA", Score: 0.99}}, nil
}

type fakeEmbedder struct{}

func (fakeEmbedder) Embed(context.Context, string) ([]float32, error) { return []float32{1, 0}, nil }
func (fakeEmbedder) ModelVersion() string                             { return "test" }

// TestProjectIsolation_HybridSearchTier1 端到端：Tier1（SurrealDB FTS + HNSW 向量）。
func TestProjectIsolation_HybridSearchTier1(t *testing.T) {
	st := seedEvents(t)
	hr := NewHybridRetrieverWithCognitive(st, nil, nil, nil, fakeCognitive{}, nil)
	hr.InjectEmbedder(fakeEmbedder{})
	assertNoMarker(t, hr, "prj_b")
	assertHasMarker(t, hr, "prj_a")
}

func search4(t *testing.T, hr *HybridRetrieverImpl, projectID string) []types.ScoredFragment {
	t.Helper()
	// Tier0 BM25 对 KV 原值（事件 JSON，Payload 为 base64）打分，故查询词同时带上
	// task_id 里的明文 token，保证两个 Tier 的召回都能命中 A、B 两条事件。
	res, err := hr.Search(context.Background(), alphaMarker+" 部署 alphatask bravotask", types.SearchScope{Type: "memory", ProjectID: projectID},
		types.RetrievalConfig{FinalTopK: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	return res
}

func assertNoMarker(t *testing.T, hr *HybridRetrieverImpl, projectID string) {
	t.Helper()
	for _, f := range search4(t, hr, projectID) {
		if strings.Contains(f.Content, alphaMarker) || f.Source == "episodic:evA" || f.Source == "evA" {
			t.Fatalf("项目 %s 检索到了项目 A 的记忆: source=%s content=%q", projectID, f.Source, f.Content)
		}
	}
}

func assertHasMarker(t *testing.T, hr *HybridRetrieverImpl, projectID string) {
	t.Helper()
	for _, f := range search4(t, hr, projectID) {
		if strings.Contains(f.Content, alphaMarker) || f.Source == "episodic:evA" || f.Source == "evA" {
			return
		}
	}
	t.Fatalf("项目 %s 应能检索到自己的记忆（过滤不能误伤本项目）", projectID)
}
