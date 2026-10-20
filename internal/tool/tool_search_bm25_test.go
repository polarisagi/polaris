package tool

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

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
	var res struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(res.Tools))
	for i, x := range res.Tools {
		names[i] = x.Name
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
		// 激活后下一轮 Schemas 带上该工具，且追加在核心之后。
		ctx := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, "s")
		ss := cc.Schemas(ctx, types.TrustUntrusted)
		found := false
		for _, s := range ss[2:] {
			if s.Name == c.want {
				found = true
			}
		}
		if !found {
			t.Errorf("query %q: %s 未被激活", c.query, c.want)
		}
	}
}

func TestToolSearch_NoMatchGivesHint(t *testing.T) {
	cc := lazySearchCatalog()
	if got := searchWith(t, cc, "s", "zzzzqqq"); len(got) != 0 {
		t.Fatalf("不应有匹配：%v", got)
	}
}
