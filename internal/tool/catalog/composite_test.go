package catalog

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// fakeSource 是测试用的静态 Catalog 源，仅实现 List/Lookup，Register/Unregister/Invalidate 为空操作。
type fakeSource struct {
	entries []protocol.CatalogEntry
}

func (f *fakeSource) List(_ context.Context, minTrust types.TrustTier) []protocol.CatalogEntry {
	var out []protocol.CatalogEntry
	for _, e := range f.entries {
		if e.TrustTier >= minTrust {
			out = append(out, e)
		}
	}
	return out
}
func (f *fakeSource) Lookup(name string) (protocol.CatalogEntry, bool) {
	for _, e := range f.entries {
		if e.Name == name {
			return e, true
		}
	}
	return protocol.CatalogEntry{}, false
}
func (f *fakeSource) Register(entry protocol.CatalogEntry) { f.entries = append(f.entries, entry) }
func (f *fakeSource) Unregister(name string) {
	for i, e := range f.entries {
		if e.Name == name {
			f.entries = append(f.entries[:i], f.entries[i+1:]...)
			return
		}
	}
}
func (f *fakeSource) Invalidate() {}
func (f *fakeSource) Schemas(ctx context.Context, minTrust types.TrustTier) []types.ToolSchema {
	list := f.List(ctx, minTrust)
	out := make([]types.ToolSchema, 0, len(list))
	for _, e := range list {
		out = append(out, types.ToolSchema{Name: e.Name, Description: e.Description, Parameters: e.Parameters})
	}
	return out
}

func coreEntry(name string) protocol.CatalogEntry {
	return protocol.CatalogEntry{Name: name, Description: "core tool " + name, Source: types.ToolBuiltin, TrustTier: types.TrustSystem}
}

func communityEntry(name string) protocol.CatalogEntry {
	return protocol.CatalogEntry{Name: name, Description: "community tool " + name, Source: types.ToolMCP, TrustTier: types.TrustCommunity}
}

// TestCompositeCatalog_ListDedupAndSort 验证多源 fan-in 去重、按 Source 权重+名称排序，
// 以及非法 LLM 工具名被防御性丢弃。
func TestCompositeCatalog_ListDedupAndSort(t *testing.T) {
	src1 := &fakeSource{entries: []protocol.CatalogEntry{coreEntry("zeta"), {Name: "bad name!", Source: types.ToolBuiltin, TrustTier: types.TrustSystem}}}
	src2 := &fakeSource{entries: []protocol.CatalogEntry{coreEntry("zeta"), communityEntry("alpha")}} // zeta 重复，应去重

	cc := NewCompositeCatalog(src1, src2)
	entries := cc.List(context.Background(), types.TrustUntrusted)

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	if len(names) != 2 {
		t.Fatalf("expected 2 deduped+filtered entries, got %d: %v", len(names), names)
	}
	// builtin(zeta) 权重低于 mcp(alpha)，应排在前面
	if names[0] != "zeta" || names[1] != "alpha" {
		t.Errorf("unexpected order: %v", names)
	}
}

// TestCompositeCatalog_Lookup 验证 Lookup 在缓存为空时触发重建，能查到已存在条目，查不到时返回 false。
func TestCompositeCatalog_Lookup(t *testing.T) {
	src := &fakeSource{entries: []protocol.CatalogEntry{coreEntry("search_files")}}
	cc := NewCompositeCatalog(src)

	entry, ok := cc.Lookup("search_files")
	if !ok || entry.Name != "search_files" {
		t.Fatalf("expected to find search_files, got ok=%v entry=%+v", ok, entry)
	}
	if _, ok := cc.Lookup("does_not_exist"); ok {
		t.Errorf("expected Lookup miss for unknown tool")
	}
}

// TestCompositeCatalog_Schemas_NoLazyLoadBelowThreshold 验证工具总数未超过
// LazyLoadThreshold 时，Schemas() 直接返回全部工具，不注入 search_tools。
func TestCompositeCatalog_Schemas_NoLazyLoadBelowThreshold(t *testing.T) {
	src := &fakeSource{entries: []protocol.CatalogEntry{coreEntry("a"), communityEntry("b")}}
	cc := NewCompositeCatalog(src)
	cc.LazyLoadThreshold = 10

	schemas := cc.Schemas(context.Background(), types.TrustUntrusted)
	if len(schemas) != 2 {
		t.Fatalf("expected 2 schemas (no lazy load), got %d", len(schemas))
	}
	for _, s := range schemas {
		if s.Name == "search_tools" {
			t.Errorf("did not expect search_tools meta-tool below threshold")
		}
	}
}

// TestCompositeCatalog_Schemas_LazyLoadCollapsesNonCoreTools 验证超过阈值且
// Embedder 非 nil 时，非 TrustSystem(4) 且未被会话激活的工具被折叠，
// 仅保留 core 工具 + search_tools 元工具（P1-2 懒加载核心行为）。
func TestCompositeCatalog_Schemas_LazyLoadCollapsesNonCoreTools(t *testing.T) {
	src := &fakeSource{entries: []protocol.CatalogEntry{coreEntry("core_tool"), communityEntry("community_tool")}}
	cc := NewCompositeCatalog(src)
	cc.LazyLoadThreshold = 1 // 2 个条目 > 阈值 1，触发懒加载
	cc.Embedder = fakeEmbedder{}

	schemas := cc.Schemas(context.Background(), types.TrustUntrusted)

	names := map[string]bool{}
	for _, s := range schemas {
		names[s.Name] = true
	}
	if !names["core_tool"] {
		t.Errorf("expected core_tool (TrustSystem) to always be present, got %v", names)
	}
	if names["community_tool"] {
		t.Errorf("expected community_tool to be collapsed under lazy load, got %v", names)
	}
	if !names["search_tools"] {
		t.Errorf("expected search_tools meta-tool to be injected under lazy load, got %v", names)
	}
}

// TestCompositeCatalog_ActivateTool_ExposesToolForSession 验证 ActivateTool
// 激活后，同一 TaskID 的 Schemas() 调用能看到该工具；不同/空 session 看不到，
// CleanupSession 后再次不可见——覆盖 tool_search.go 依赖的会话级激活协议。
func TestCompositeCatalog_ActivateTool_ExposesToolForSession(t *testing.T) {
	src := &fakeSource{entries: []protocol.CatalogEntry{coreEntry("core_tool"), communityEntry("community_tool")}}
	cc := NewCompositeCatalog(src)
	cc.LazyLoadThreshold = 1
	cc.Embedder = fakeEmbedder{}

	sessionID := "task-123"
	cc.ActivateTool(sessionID, "community_tool")

	ctxWithSession := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, sessionID)
	schemas := cc.Schemas(ctxWithSession, types.TrustUntrusted)
	found := false
	for _, s := range schemas {
		if s.Name == "community_tool" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected community_tool to be visible after ActivateTool for the same session")
	}

	// 不同 session 看不到该激活。
	otherCtx := context.WithValue(context.Background(), protocol.CtxTaskIDKey{}, "other-session")
	otherSchemas := cc.Schemas(otherCtx, types.TrustUntrusted)
	for _, s := range otherSchemas {
		if s.Name == "community_tool" {
			t.Errorf("did not expect community_tool visible for an unrelated session")
		}
	}

	// CleanupSession 后，原 session 也看不到了。
	cc.CleanupSession(sessionID)
	afterCleanup := cc.Schemas(ctxWithSession, types.TrustUntrusted)
	for _, s := range afterCleanup {
		if s.Name == "community_tool" {
			t.Errorf("expected community_tool hidden again after CleanupSession")
		}
	}
}

// TestCompositeCatalog_Invalidate 验证 Invalidate 清空缓存后下次 List 会重新拉取源数据（含新注册项）。
func TestCompositeCatalog_Invalidate(t *testing.T) {
	src := &fakeSource{entries: []protocol.CatalogEntry{coreEntry("a")}}
	cc := NewCompositeCatalog(src)

	if len(cc.List(context.Background(), types.TrustUntrusted)) != 1 {
		t.Fatalf("expected 1 entry before mutation")
	}

	src.entries = append(src.entries, communityEntry("b"))
	// 缓存未失效前，List 仍返回旧结果。
	if len(cc.List(context.Background(), types.TrustUntrusted)) != 1 {
		t.Errorf("expected cached result (1 entry) before Invalidate")
	}

	cc.Invalidate()
	if len(cc.List(context.Background(), types.TrustUntrusted)) != 2 {
		t.Errorf("expected 2 entries after Invalidate triggers rebuild")
	}
}

// fakeEmbedder 满足 search.Embedder 接口，仅用于让 CompositeCatalog 判定
// "懒加载可用"（Schemas() 只检查 Embedder != nil，不实际调用 Embed）。
type fakeEmbedder struct{}

func (fakeEmbedder) Embed(_ string) []float32 { return []float32{0.1, 0.2, 0.3} }
