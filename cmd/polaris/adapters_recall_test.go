package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type fakeFTS struct {
	hits    []store.ScoredID
	err     error
	gotK    int
	gotText string
}

func (f *fakeFTS) FTSSearch(query string, k int) ([]store.ScoredID, error) {
	f.gotText, f.gotK = query, k
	return f.hits, f.err
}

type fakeEntities struct {
	byKey map[string]*types.Entity // "type\x00name"
	calls int
}

func (f *fakeEntities) GetEntity(_ context.Context, typ, name string) (*types.Entity, error) {
	f.calls++
	if e, ok := f.byKey[typ+"\x00"+name]; ok {
		return e, nil
	}
	return nil, apperr.New(apperr.CodeNotFound, "Entity not found")
}

func entKey(typ, name string) string { return typ + "\x00" + name }

var recallNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func newTestRecallAdapter(f *fakeFTS, e *fakeEntities) *recallCognitiveAdapter {
	a := newRecallCognitiveAdapter(f, e)
	a.now = func() time.Time { return recallNow }
	return a
}

// TestProjectIsolation_RecallCognitiveAdapter 读取面 P3/P4 的 L2 来源：共享 FTS 里项目 A 的情景事件、
// 反思洞察、扩展目录条目都不得经 L2 适配器出现——适配器只放行全局层的语义实体。
// 由 tools/memory_isolation_check.go 纳入门控（ADR-0097 决策三修订）。
func TestProjectIsolation_RecallCognitiveAdapter(t *testing.T) {
	fts := &fakeFTS{hits: []store.ScoredID{
		{ID: "0b6f2c1e-ALPHA-EVENT", Score: 9}, // 项目 A 的情景事件（docID=Event.ID）
		{ID: "her_task-alpha", Score: 8},       // 反思洞察
		{ID: "ext_some-extension", Score: 7},   // 扩展目录
		{ID: "sement_Tool_go", Score: 3},       // 全局语义实体
	}}
	ents := &fakeEntities{byKey: map[string]*types.Entity{
		entKey("Tool", "go"): {Name: "go", Type: "Tool", Properties: map[string]any{"description": "Go 1.26"}},
	}}
	got, err := newTestRecallAdapter(fts, ents).FTSSearch(context.Background(), "部署", 5)
	require.NoError(t, err)
	require.Len(t, got, 1, "情景/反思/扩展命中必须被丢弃，不得因其项目归属未知而放行")
	require.Equal(t, "sement_Tool_go", got[0].DocID)
	require.Equal(t, "go: Go 1.26", got[0].Snippet)
	require.Equal(t, 1, ents.calls, "不得为非实体命中回取任何正文")
}

func TestRecallAdapter_OverfetchesAndCapsToK(t *testing.T) {
	fts := &fakeFTS{}
	ents := &fakeEntities{byKey: map[string]*types.Entity{}}
	for _, n := range []string{"a", "b", "c", "d"} {
		fts.hits = append(fts.hits, store.ScoredID{ID: "sement_Fact_" + n, Score: 1})
		ents.byKey[entKey("Fact", n)] = &types.Entity{Name: n, Type: "Fact"}
	}
	got, err := newTestRecallAdapter(fts, ents).FTSSearch(context.Background(), "q", 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, 2*entityFTSOverfetch, fts.gotK, "实体命中被情景事件挤占名次，须放大取回量")
	require.Equal(t, "q", fts.gotText)
}

func TestRecallAdapter_IDSplitAndTaint(t *testing.T) {
	ents := &fakeEntities{byKey: map[string]*types.Entity{
		entKey("tool_pref", "editor"):  {Name: "editor", Type: "tool_pref", TaintLevel: types.TaintHigh, Properties: map[string]any{"value": "vim"}},
		entKey("Tool", "go_fmt_rules"): {Name: "go_fmt_rules", Type: "Tool", TaintLevel: types.TaintLow, Properties: map[string]any{"description": "gofmt"}},
	}}
	fts := &fakeFTS{hits: []store.ScoredID{
		{ID: "sement_tool_pref_editor", Score: 5},  // type 含下划线
		{ID: "sement_Tool_go_fmt_rules", Score: 4}, // name 含下划线
		{ID: "sement_Fact_no_such_split", Score: 3},
		{ID: "sement_", Score: 2},
		{ID: "sement_Nounderscore", Score: 1},
	}}
	got, err := newTestRecallAdapter(fts, ents).FTSSearch(context.Background(), "q", 10)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, types.TaintHigh, got[0].Taint, "污点随实体返回，供召回按 MaxTaint 过滤")
	require.Equal(t, `editor: {"value":"vim"}`, got[0].Snippet)
	require.Equal(t, types.TaintLow, got[1].Taint)
	require.Equal(t, "go_fmt_rules: gofmt", got[1].Snippet)
}

func TestRecallAdapter_SkipsInactiveEntities(t *testing.T) {
	ms := func(d time.Duration) int64 { return recallNow.Add(d).UnixMilli() }
	ents := &fakeEntities{byKey: map[string]*types.Entity{
		entKey("F", "active"):      {Name: "active", Type: "F", Status: "active"},
		entKey("F", "empty_state"): {Name: "empty_state", Type: "F"},
		entKey("F", "superseded"):  {Name: "superseded", Type: "F", Status: "superseded"},
		entKey("F", "expired"):     {Name: "expired", Type: "F", Status: "active", ValidUntil: ms(-time.Hour)},
		entKey("F", "future"):      {Name: "future", Type: "F", Status: "active", ValidFrom: ms(time.Hour)},
		entKey("F", "window_ok"):   {Name: "window_ok", Type: "F", Status: "active", ValidFrom: ms(-time.Hour), ValidUntil: ms(time.Hour)},
		entKey("F", "from_rag"):    {Name: "from_rag", Type: "F", Status: "active", SourceType: "graphrag_ingest"},
	}}
	fts := &fakeFTS{}
	for _, n := range []string{"active", "empty_state", "superseded", "expired", "future", "window_ok", "from_rag"} {
		fts.hits = append(fts.hits, store.ScoredID{ID: "sement_F_" + n, Score: 1})
	}
	got, err := newTestRecallAdapter(fts, ents).FTSSearch(context.Background(), "q", 10)
	require.NoError(t, err)
	names := make([]string, 0, len(got))
	for _, g := range got {
		names = append(names, g.DocID)
	}
	require.Equal(t, []string{"sement_F_active", "sement_F_empty_state", "sement_F_window_ok"}, names,
		"已被取代/过期/未生效的实体与 RAG 抽取实体不得召回（FTS 索引不随状态变更删除）")
}

func TestRecallAdapter_ErrorsAndNilSafe(t *testing.T) {
	boom := errors.New("fts down")
	_, err := newTestRecallAdapter(&fakeFTS{err: boom}, &fakeEntities{}).FTSSearch(context.Background(), "q", 3)
	require.ErrorIs(t, err, boom, "FTS 失败原样上抛，由召回按无结果降级")

	got, err := newRecallCognitiveAdapter(nil, nil).FTSSearch(context.Background(), "q", 3)
	require.NoError(t, err)
	require.Empty(t, got)
	got, err = newTestRecallAdapter(&fakeFTS{}, &fakeEntities{}).FTSSearch(context.Background(), "q", 0)
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestEntitySnippet_Deterministic(t *testing.T) {
	e := &types.Entity{Name: "n", Properties: map[string]any{"z": 1, "a": "x", "source_type": "llm_extract", "valid_from": 5}}
	want := `n: {"a":"x","z":1}`
	for i := 0; i < 20; i++ {
		require.Equal(t, want, entitySnippet(e))
	}
	require.Equal(t, "n", entitySnippet(&types.Entity{Name: "n"}))
	require.Equal(t, "n", entitySnippet(&types.Entity{Name: "n", Properties: map[string]any{"source_type": "x"}}))
}

// FTSEpisodic 只返回可能是情景事件的命中 ID + BM25 分（保持降序）；实体/反思/扩展目录由各自来源负责，
// 不得经情景路径重复进入 prompt；不取任何正文（ADR-0105 决策十 WP11）。
func TestRecallAdapter_FTSEpisodicFiltersNonEpisodic(t *testing.T) {
	fts := &fakeFTS{hits: []store.ScoredID{
		{ID: "ev-1", Score: 9},
		{ID: "sement_Tool_go", Score: 8},
		{ID: "her_task-1", Score: 7},
		{ID: "ext_x", Score: 6},
		{ID: "ev-2", Score: 5},
	}}
	ents := &fakeEntities{byKey: map[string]*types.Entity{}}
	got, err := newTestRecallAdapter(fts, ents).FTSEpisodic(context.Background(), "部署", 8)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "ev-1", got[0].DocID)
	require.Equal(t, float32(9), got[0].Score)
	require.Equal(t, "ev-2", got[1].DocID)
	require.Equal(t, 8, fts.gotK, "k 原样下传，放大由调用方负责")
	require.Equal(t, 0, ents.calls)
}

func TestRecallAdapter_FTSEpisodicErrorsAndNilSafe(t *testing.T) {
	boom := errors.New("fts down")
	_, err := newTestRecallAdapter(&fakeFTS{err: boom}, &fakeEntities{}).FTSEpisodic(context.Background(), "q", 3)
	require.ErrorIs(t, err, boom)
	got, err := newTestRecallAdapter(&fakeFTS{}, &fakeEntities{}).FTSEpisodic(context.Background(), "q", 0)
	require.NoError(t, err)
	require.Empty(t, got)
	got, err = newRecallCognitiveAdapter(nil, nil).FTSEpisodic(context.Background(), "q", 3)
	require.NoError(t, err)
	require.Empty(t, got, "Tier0 无 SurrealDB：返回空")
}
