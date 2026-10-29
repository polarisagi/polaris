package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/memory/testutil"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// 以下 TestProjectIsolation* 用例由 tools/memory_isolation_check.go 纳入 make lint（ADR-0097 决策三修订）。

// TestProjectIsolation_EpisodicQuery 读取面 P1/P2/P5 的底层过滤点：EpisodicQuery.ProjectID。
func TestProjectIsolation_EpisodicQuery(t *testing.T) {
	ctx := context.Background()
	mem := NewEpisodicMem(testutil.NewMockStore())

	mustAppend(t, mem, ctx, types.Event{ID: "a1", TaskID: "sA", ProjectID: "prj_a", Payload: []byte("ALPHA-MARKER 数据库迁移方案")})
	mustAppend(t, mem, ctx, types.Event{ID: "b1", TaskID: "sB", ProjectID: "prj_b", Payload: []byte("BRAVO 数据库迁移方案")})
	mustAppend(t, mem, ctx, types.Event{ID: "d1", TaskID: "sD", Payload: []byte("DEFAULT 数据库迁移方案")}) // 未打标 = 默认项目

	got := queryPayloads(t, mem, types.EpisodicQuery{Topics: []string{"数据库迁移"}, ProjectID: "prj_b", MaxTaintLevel: types.TaintHigh})
	if len(got) != 1 || !strings.Contains(got[0], "BRAVO") {
		t.Fatalf("项目 B 只应看到自己的事件，got %v", got)
	}
	got = queryPayloads(t, mem, types.EpisodicQuery{Topics: []string{"数据库迁移"}, ProjectID: types.DefaultProjectID, MaxTaintLevel: types.TaintHigh})
	if len(got) != 1 || !strings.Contains(got[0], "DEFAULT") {
		t.Fatalf("默认项目只应看到未打标事件，got %v", got)
	}
	// 空 ProjectID = 后台/系统读取，不限项目。
	if got = queryPayloads(t, mem, types.EpisodicQuery{Topics: []string{"数据库迁移"}, MaxTaintLevel: types.TaintHigh}); len(got) != 3 {
		t.Fatalf("不限项目时应返回全部 3 条，got %v", got)
	}
}

// TestProjectIsolation_AppendTagsFromCtx 写入方未显式打标时，以执行 ctx 注入的项目兜底；
// 显式打标优先于 ctx。
func TestProjectIsolation_AppendTagsFromCtx(t *testing.T) {
	st := testutil.NewMockStore()
	mem := NewEpisodicMem(st)
	ctx := protocol.WithProjectID(context.Background(), "prj_ctx")

	mustAppend(t, mem, ctx, types.Event{ID: "e1", Payload: []byte("x")})
	mustAppend(t, mem, ctx, types.Event{ID: "e2", ProjectID: "prj_explicit", Payload: []byte("y")})

	if p, ok := mem.ProjectOf(context.Background(), "e1"); !ok || p != "prj_ctx" {
		t.Fatalf("未打标事件应取 ctx 项目，got %q ok=%v", p, ok)
	}
	if p, ok := mem.ProjectOf(context.Background(), "episodic:e2"); !ok || p != "prj_explicit" {
		t.Fatalf("显式打标优先，got %q ok=%v", p, ok)
	}
	if _, ok := mem.ProjectOf(context.Background(), "sement_Tool_go"); ok {
		t.Fatal("非情景事件应返回 isEpisodic=false")
	}
}

// TestProjectIsolation_DurativeClustersPerProject 持续簇按项目分桶聚类：簇摘要由 LLM 从成员
// 原文生成，混簇会把 A 项目内容摘进 B 可见的簇。
func TestProjectIsolation_DurativeClustersPerProject(t *testing.T) {
	ctx := context.Background()
	st := notFoundStore{testutil.NewMockStore()}
	ep := NewEpisodicMem(st)
	for i, pid := range []string{"prj_a", "prj_b", "prj_a", "prj_b", "prj_a", "prj_b"} {
		mustAppend(t, ep, ctx, types.Event{
			ID: "ev" + string(rune('0'+i)), TaskID: "s", ProjectID: pid,
			Payload: []byte("step"), CreatedAt: time.Now(),
		})
	}
	provider := &testutil.MockProvider{Resp: `{"is_continuous": true, "summary": "s", "label": "l"}`}
	dm := NewDurativeMemoryManager(ep, provider, st)
	if err := dm.Consolidate(ctx); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	groups := dm.ListGroups(ctx, "", 10)
	if len(groups) != 2 {
		t.Fatalf("两个项目各 3 条事件应各成一簇，got %d 簇", len(groups))
	}
	for _, g := range groups {
		for _, id := range g.EventIDs {
			owner, _ := ep.ProjectOf(ctx, id)
			if owner != g.EffectiveProjectID() {
				t.Fatalf("簇 %s（项目 %s）混入了项目 %s 的事件 %s", g.ID, g.EffectiveProjectID(), owner, id)
			}
		}
		raw, _ := st.Get(ctx, []byte("durative_group:"+g.ID))
		var stored DurativeGroup
		_ = json.Unmarshal(raw, &stored)
		if stored.ProjectID == "" {
			t.Fatalf("簇 %s 未持久化项目归属", g.ID)
		}
	}
}

// notFoundStore 让缺失键返回 apperr.ErrNotFound，与生产 SQLiteStore.Get 同语义
// （testutil.MockStore 返回 nil,nil，Durative 会误判"所有事件都已入簇"而从不聚类）。
type notFoundStore struct{ *testutil.MockStore }

func (s notFoundStore) Get(ctx context.Context, key []byte) ([]byte, error) {
	v, err := s.MockStore.Get(ctx, key)
	if err == nil && v == nil {
		return nil, apperr.ErrNotFound
	}
	return v, err
}

func mustAppend(t *testing.T, mem *EpisodicMem, ctx context.Context, ev types.Event) {
	t.Helper()
	if err := mem.Append(ctx, ev, types.TaintNone); err != nil {
		t.Fatalf("Append %s: %v", ev.ID, err)
	}
}

func queryPayloads(t *testing.T, mem *EpisodicMem, q types.EpisodicQuery) []string {
	t.Helper()
	res, err := mem.Query(context.Background(), q)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	out := make([]string, 0, len(res))
	for _, r := range res {
		if ev := r.EventPtr(); ev != nil {
			out = append(out, string(ev.Payload))
		}
	}
	return out
}

// TestEpisodicQuery_ByIDs 回合内召回用：按 FTS 选出的 ID 直取，保持 ID 顺序，且项目/污点/会话过滤仍然生效
// （ID 来自共享索引，不能信任调用方已筛过）。陈旧/不存在的 ID 静默跳过。
func TestEpisodicQuery_ByIDs(t *testing.T) {
	ctx := context.Background()
	mem := NewEpisodicMem(testutil.NewMockStore())
	mustAppend(t, mem, ctx, types.Event{ID: "a1", ProjectID: "prj_a", Payload: []byte("ALPHA")})
	mustAppend(t, mem, ctx, types.Event{ID: "b1", ProjectID: "prj_b", Payload: []byte("BRAVO")})
	mustAppend(t, mem, ctx, types.Event{ID: "b2", ProjectID: "prj_b", TaintLevel: types.TaintHigh, Payload: []byte("BRAVO-TAINTED")})
	mustAppend(t, mem, ctx, types.Event{ID: "b3", ProjectID: "prj_b", Payload: []byte("BRAVO-3")})

	ids := []string{"b3", "missing", "a1", "episodic:b1", "b2", "b3"}
	got := queryPayloads(t, mem, types.EpisodicQuery{IDs: ids, ProjectID: "prj_b", MaxTaintLevel: types.TaintLow})
	if len(got) != 2 || got[0] != "BRAVO-3" || got[1] != "BRAVO" {
		t.Fatalf("应按 IDs 顺序只返回项目 B 且污点不超限、去重后的事件，got %v", got)
	}
	got = queryPayloads(t, mem, types.EpisodicQuery{IDs: ids, ProjectID: "prj_b", MaxTaintLevel: types.TaintHigh, K: 2})
	if len(got) != 2 || got[0] != "BRAVO-3" || got[1] != "BRAVO" {
		t.Fatalf("K 截断应保持 IDs 顺序，got %v", got)
	}
}
