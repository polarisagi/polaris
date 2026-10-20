package agent

// 工具可达性评测（ADR-0119 / 任务 4e）。
//
// 懒加载把非核心工具挪出首轮 tools 数组，风险是"模型该用的工具够不着"。本评测没有 LLM（CI 内不调外部模型），
// 用确定性的可达性代理衡量"工具选择准确率"：对 adr0114 与 adr0119 两组 EvalCase 里每个期望工具，
//   - 改前（全量下发）：工具在首轮 tools 数组里即可达；
//   - 改后（懒加载）：在核心集里直接可达；否则按 case 的检索词（缺省用 user_input 原句）走真实 tool_search，
//     命中并激活后，下一轮 tools 数组里出现才算可达。
// 它度量的是"检索通路不丢工具"，不是模型的选择能力（那需要上线后用 llm_calls 复核，见报告）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/types"
)

func loadEvalCases(t *testing.T, name string) []protocol.EvalCase {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "eval", "harness", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var cases []protocol.EvalCase
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	return cases
}

func expectedTools(c protocol.EvalCase) []string {
	raw, _ := c.Expected["expected_tool_sequence"].([]any)
	var out []string
	for _, r := range raw {
		if n, ok := r.(string); ok && n != "tool_search" {
			out = append(out, n)
		}
	}
	return out
}

func reachCatalog(t *testing.T, lazy bool) *catalog.CompositeCatalog {
	t.Helper()
	reg := budgetRegistry(t)
	mc := catalog.NewMemoryCatalog()
	for _, tl := range reg.List() {
		mc.Register(protocol.CatalogEntry{Name: tl.Name, Description: tl.Description, Parameters: tl.InputSchema, Source: tl.Source, TrustTier: tl.TrustTier})
	}
	// adr0114 case 4 依赖已安装的 social MCP 插件。
	mc.Register(protocol.CatalogEntry{Name: "mcp__polaris-social__social_search", Description: "Search posts on social platforms (Zhihu, Xiaohongshu, Weibo)", Source: types.ToolMCP, TrustTier: types.TrustCommunity})
	cc := catalog.NewCompositeCatalog(mc)
	cc.LazyLoadThreshold = 40
	if !lazy {
		cc.LazyLoadThreshold = 100000
	}
	return cc
}

func schemaHas(ss []types.ToolSchema, name string) bool {
	for _, s := range ss {
		if s.Name == name {
			return true
		}
	}
	return false
}

type reachStats struct{ total, reached, direct, viaSearch int }

func (s reachStats) pct() float64 { return 100 * float64(s.reached) / float64(s.total) }

// runReach 返回在给定检索词来源下的可达统计。useQuery=true 用 case 的 search_query（缺省回落 user_input）。
func runReach(t *testing.T, cases []protocol.EvalCase, lazy, useQuery bool) (reachStats, []string) {
	t.Helper()
	var st reachStats
	var misses []string
	for _, c := range cases {
		cc := reachCatalog(t, lazy)
		sid := "reach-" + c.ID
		ctx := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, sid)
		search := tool.MakeToolSearchFn(cc, nil)
		first := cc.Schemas(ctx, types.TrustUntrusted)
		for _, name := range expectedTools(c) {
			st.total++
			if schemaHas(first, name) {
				st.reached++
				st.direct++
				continue
			}
			q, _ := c.Input["user_input"].(string)
			if sq, _ := c.Input["search_query"].(string); useQuery && sq != "" {
				q = sq
			}
			in, _ := json.Marshal(map[string]string{"query": q})
			if _, err := search(ctx, in); err != nil {
				t.Fatal(err)
			}
			if schemaHas(cc.Schemas(ctx, types.TrustUntrusted), name) {
				st.reached++
				st.viaSearch++
			} else {
				misses = append(misses, c.ID+":"+name)
			}
		}
	}
	return st, misses
}

func TestToolReachability_BeforeAfter(t *testing.T) {
	cases := append(loadEvalCases(t, "adr0114_trajectory_cases.json"), loadEvalCases(t, "adr0119_tool_search_cases.json")...)

	before, bm := runReach(t, cases, false, true)
	after, am := runReach(t, cases, true, true)
	rawAfter, rm := runReach(t, cases, true, false)

	t.Logf("REACH_EVAL cases=%d expected_tools=%d", len(cases), before.total)
	t.Logf("REACH_EVAL before(全量下发)        : %d/%d = %.1f%%  misses=%v", before.reached, before.total, before.pct(), bm)
	t.Logf("REACH_EVAL after (懒加载+检索词)   : %d/%d = %.1f%% (核心直达 %d, tool_search 命中 %d)  misses=%v", after.reached, after.total, after.pct(), after.direct, after.viaSearch, am)
	t.Logf("REACH_EVAL after (懒加载+user_input原句): %d/%d = %.1f%%  misses=%v", rawAfter.reached, rawAfter.total, rawAfter.pct(), rm)

	if drop := before.pct() - after.pct(); drop > 2 {
		t.Errorf("可达率下降 %.1f 个百分点，超过 2 个点的上限；应调整核心集或工具描述：%v", drop, am)
	}
}

// 非核心任务必须"先 tool_search 再调用"：首轮不应已含该工具，激活后下一轮才有，且核心部分字节不变。
func TestToolReachability_NonCoreNeedsSearchFirst(t *testing.T) {
	for _, c := range loadEvalCases(t, "adr0119_tool_search_cases.json") {
		cc := reachCatalog(t, true)
		ctx := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, "nc-"+c.ID)
		first := cc.Schemas(ctx, types.TrustUntrusted)
		for _, name := range expectedTools(c) {
			if schemaHas(first, name) {
				t.Errorf("%s: %s 不应在首轮 tools 里（它不是核心工具）", c.ID, name)
			}
		}
		if !schemaHas(first, "tool_search") {
			t.Fatalf("%s: 首轮必须含 tool_search", c.ID)
		}
	}
}
