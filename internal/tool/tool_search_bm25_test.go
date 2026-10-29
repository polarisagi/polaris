package tool

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestSearchTokens(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"memory_search", []string{"memory", "search"}},
		{"Web-Fetch 2", []string{"web", "fetch", "2"}},
		{"定时任务", []string{"定", "时", "定时", "任", "时任", "务", "任务"}},
	}
	for _, c := range cases {
		if got := searchTokens(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("searchTokens(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func searchCorpus() []protocol.CatalogEntry {
	mk := func(n, d string) protocol.CatalogEntry {
		return protocol.CatalogEntry{Name: n, Description: d, Source: types.ToolBuiltin, TrustTier: types.TrustSystem}
	}
	return []protocol.CatalogEntry{
		mk("read_file", "读取文件内容"),
		mk("tool_search", "搜索并激活工具"),
		mk("automation_save", "保存定时任务或自动化工作流"),
		mk("image_generate", "Generate an image from a text prompt"),
		mk("switch_model", "切换当前会话使用的模型"),
		mk("browser_navigate", "Open a URL in the browser"),
	}
}

func searchWith(t *testing.T, cc *catalog.CompositeCatalog, session, query string) []string {
	t.Helper()
	fn := MakeToolSearchFn(cc, nil)
	ctx := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, session)
	b, _ := json.Marshal(map[string]string{"query": query})
	out, err := fn(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	return parseSearchNames(string(out))
}

// parseSearchNames 从 tool_search 的文本输出里取命中工具名：每段 "名称: 描述\nparams: ..."，
// 末段是调用说明（ADR-0120 决策三）。
func parseSearchNames(out string) []string {
	var names []string
	for _, block := range strings.Split(out, "\n\n") {
		if !strings.Contains(block, "\nparams: ") {
			continue
		}
		if i := strings.Index(block, ": "); i > 0 {
			names = append(names, block[:i])
		}
	}
	return names
}

func lazySearchCatalog() *catalog.CompositeCatalog {
	mc := catalog.NewMemoryCatalog()
	for _, e := range searchCorpus() {
		mc.Register(e)
	}
	cc := catalog.NewCompositeCatalog(mc)
	cc.LazyLoadThreshold = 1
	cc.CoreTools = []string{"read_file", "tool_search"}
	return cc
}

// 无 Embedder 也能按关键词找到并激活非核心工具；核心工具不占结果名额。
func TestToolSearch_KeywordWithoutEmbedder(t *testing.T) {
	cases := []struct{ query, want string }{
		{"定时任务", "automation_save"},
		{"生成图片 image", "image_generate"},
		{"切换模型", "switch_model"},
		{"open url browser", "browser_navigate"},
		{"switch_model", "switch_model"},
	}
	for _, c := range cases {
		cc := lazySearchCatalog()
		got := searchWith(t, cc, "s", c.query)
		if len(got) == 0 || got[0] != c.want {
			t.Errorf("query %q: got %v want first %s", c.query, got, c.want)
		}
		for _, n := range got {
			if n == "read_file" || n == "tool_search" {
				t.Errorf("query %q: 核心工具不应出现在结果里：%v", c.query, got)
			}
		}
		// 默认 in_history：tools 数组不变（只有核心工具），模型经 call_tool 调用（ADR-0120 决策三）。
		ctx := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, "s")
		if ss := cc.Schemas(ctx, types.TrustUntrusted); len(ss) != 2 {
			t.Errorf("query %q: in_history 模式下 tools 数组不应变化，got %d 个", c.query, len(ss))
		}
	}
}

// 结果直接带参数 schema 与 call_tool 用法；点名精确匹配只返回该工具。
func TestToolSearch_ReturnsParamsAndExactMatch(t *testing.T) {
	cc := lazySearchCatalog()
	fn := MakeToolSearchFn(cc, nil)
	out, err := fn(context.Background(), []byte(`{"query":"switch_model"}`))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if names := parseSearchNames(got); len(names) != 1 || names[0] != "switch_model" {
		t.Fatalf("点名应只返回该工具: %v\n%s", names, got)
	}
	if !strings.Contains(got, "params: ") || !strings.Contains(got, "call_tool") {
		t.Fatalf("应返回参数与 call_tool 用法:\n%s", got)
	}
}

// tools_array 模式保留旧行为：命中工具激活进下一轮 tools 数组。
func TestToolSearch_ToolsArrayModeActivates(t *testing.T) {
	prev := config.Get()
	c := config.Config{Thresholds: config.DefaultThresholds()}
	c.Thresholds.M7Tool.LazyActivation = "tools_array"
	config.Update(&c)
	t.Cleanup(func() { config.Update(prev) })

	cc := lazySearchCatalog()
	_ = searchWith(t, cc, "s", "switch_model")
	ctx := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, "s")
	found := false
	for _, s := range cc.Schemas(ctx, types.TrustUntrusted) {
		if s.Name == "switch_model" {
			found = true
		}
	}
	if !found {
		t.Fatal("tools_array 模式下应激活进 tools 数组")
	}
}

func TestToolSearch_NoMatchGivesHint(t *testing.T) {
	cc := lazySearchCatalog()
	if got := searchWith(t, cc, "s", "zzzzqqq"); len(got) != 0 {
		t.Fatalf("不应有匹配：%v", got)
	}
	out, _ := MakeToolSearchFn(cc, nil)(context.Background(), []byte(`{"query":"zzzzqqq"}`))
	if !strings.Contains(string(out), "Closest names") {
		t.Fatalf("无命中应给出最接近的名称: %s", out)
	}
}
