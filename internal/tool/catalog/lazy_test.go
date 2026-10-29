package catalog

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

func builtinEntry(name, desc string) protocol.CatalogEntry {
	return protocol.CatalogEntry{Name: name, Description: desc, Source: types.ToolBuiltin, TrustTier: types.TrustSystem}
}

func hintCatalog() *CompositeCatalog {
	src := &fakeSource{entries: []protocol.CatalogEntry{
		builtinEntry("read_file", "读取文件内容。很长的细节"),
		builtinEntry("tool_search", "搜索并激活工具"),
		builtinEntry("switch_model", "切换当前会话使用的模型。细节"),
		builtinEntry("image_generate", "Generate an image from a prompt. details"),
		{Name: "mcp__fs__list", Description: "x", Source: types.ToolMCP, TrustTier: types.TrustCommunity},
		{Name: "weather", Description: "x", Source: types.ToolSkill, TrustTier: types.TrustCommunity},
	}}
	cc := NewCompositeCatalog(src)
	cc.LazyLoadThreshold = 1
	cc.CoreTools = []string{"read_file", "tool_search"}
	return cc
}

func TestToolsHint_ListsDeferredOnly(t *testing.T) {
	cc := hintCatalog()
	h := cc.ToolsHint(context.Background())
	for _, want := range []string{"switch_model 下一轮切换模型或思考档", "image_generate Generate an image from a prompt", "fs(list)", "weather", "call_tool {"} {
		if !strings.Contains(h, want) {
			t.Errorf("hint 缺 %q：\n%s", want, h)
		}
	}
	if strings.Contains(h, "read_file") {
		t.Errorf("核心工具已在 function 列表里，不应出现在索引：\n%s", h)
	}
	if strings.Contains(h, "很长的细节") {
		t.Errorf("简述应只取第一句：\n%s", h)
	}
}

// 字节稳定：激活不改变索引（索引只随工具集变化）。
func TestToolsHint_StableAcrossActivation(t *testing.T) {
	cc := hintCatalog()
	ctx := sessionCtx("s")
	a := cc.ToolsHint(ctx)
	cc.ActivateTool("s", "switch_model")
	if b := cc.ToolsHint(ctx); a != b {
		t.Fatalf("激活后索引字节变了：\n%s\n---\n%s", a, b)
	}
}

func TestToolsHint_EmptyWhenNotLazy(t *testing.T) {
	cc := hintCatalog()
	cc.LazyLoadThreshold = 100
	if h := cc.ToolsHint(context.Background()); h != "" {
		t.Fatalf("非懒加载应返回空串：%s", h)
	}
}

// 没有 tool_search 就无从激活：宁可全量下发。
func TestLazyActive_RequiresToolSearch(t *testing.T) {
	cc := NewCompositeCatalog(&fakeSource{entries: []protocol.CatalogEntry{
		builtinEntry("a", "x"), builtinEntry("b", "y"), builtinEntry("c", "z"),
	}})
	cc.LazyLoadThreshold = 1
	if cc.LazyActive(context.Background(), types.TrustUntrusted) {
		t.Fatal("无 tool_search 时不应启用懒加载")
	}
	if got := len(cc.Schemas(context.Background(), types.TrustUntrusted)); got != 3 {
		t.Fatalf("应全量下发 3 个，got %d", got)
	}
}

func TestDefaultCoreSet_Bounded(t *testing.T) {
	names := CoreToolNames()
	if len(names) > 15 {
		t.Fatalf("核心集应 ≤15，got %d", len(names))
	}
	cc := NewCompositeCatalog()
	if !cc.IsCore("tool_search") || cc.IsCore("switch_model") {
		t.Fatal("默认核心集应含 tool_search、不含 switch_model")
	}
}

// 英文描述按词边界截断，不再截出半个单词（ADR-0120 决策三第 6 条）。
func TestToolBrief_WordBoundary(t *testing.T) {
	got := toolBrief("Trigger an explicit reflection on past events to generate high-level insights and lessons for later reuse")
	if !strings.HasSuffix(got, "…") || strings.HasSuffix(strings.TrimSuffix(got, "…"), "reus") {
		t.Fatalf("应在词边界截断: %q", got)
	}
	if strings.Contains(got, "re…") && !strings.Contains(got, "reflection") {
		t.Fatalf("截断过短: %q", got)
	}
}
