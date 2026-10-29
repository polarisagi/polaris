package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ── §1 _meta 解析（ParseToolUI）────────────────────────────────────────────

func TestParseToolUI_StandardField(t *testing.T) {
	ui := ParseToolUI([]byte(`{"ui":{"resourceUri":"ui://s/dash","visibility":["app"]}}`), "t", "s")
	if ui == nil || ui.ResourceURI != "ui://s/dash" || len(ui.Visibility) != 1 || ui.Visibility[0] != "app" {
		t.Fatalf("standard field: %+v", ui)
	}
}

func TestParseToolUI_DeprecatedAlias(t *testing.T) {
	ui := ParseToolUI([]byte(`{"ui/resourceUri":"ui://s/dash"}`), "t", "s")
	if ui == nil || ui.ResourceURI != "ui://s/dash" {
		t.Fatalf("deprecated alias: %+v", ui)
	}
}

func TestParseToolUI_OpenAIAlias(t *testing.T) {
	ui := ParseToolUI([]byte(`{"openai/outputTemplate":"ui://s/dash"}`), "t", "s")
	if ui == nil || ui.ResourceURI != "ui://s/dash" || len(ui.Visibility) != 1 || ui.Visibility[0] != "model" {
		t.Fatalf("openai alias default (model-only, chatgpt semantics): %+v", ui)
	}
	ui2 := ParseToolUI([]byte(`{"openai/outputTemplate":"ui://s/dash","openai/widgetAccessible":true}`), "t", "s")
	if ui2 == nil || len(ui2.Visibility) != 2 {
		t.Fatalf("widgetAccessible must add app visibility: %+v", ui2)
	}
}

func TestParseToolUI_StandardOverridesAlias(t *testing.T) {
	ui := ParseToolUI([]byte(`{"ui":{"resourceUri":"ui://standard"},"openai/outputTemplate":"ui://alias","ui/resourceUri":"ui://deprecated"}`), "t", "s")
	if ui == nil || ui.ResourceURI != "ui://standard" {
		t.Fatalf("standard field must win over both aliases: %+v", ui)
	}
}

func TestParseToolUI_NonUISchemeIgnored(t *testing.T) {
	ui := ParseToolUI([]byte(`{"ui":{"resourceUri":"https://not-ui-scheme"}}`), "t", "s")
	if ui != nil {
		t.Fatalf("non ui:// resourceUri must be dropped entirely (no visibility override): %+v", ui)
	}
	// 非 ui:// 但同时声明了非默认 visibility：resourceUri 丢弃，visibility 部分保留。
	ui2 := ParseToolUI([]byte(`{"ui":{"resourceUri":"https://x","visibility":["app"]}}`), "t", "s")
	if ui2 == nil || ui2.ResourceURI != "" || len(ui2.Visibility) != 1 {
		t.Fatalf("visibility must survive resourceUri rejection: %+v", ui2)
	}
}

func TestToolUIVisibleTo_DefaultsBothWhenUnset(t *testing.T) {
	if !ToolUIVisibleTo(nil, "model") || !ToolUIVisibleTo(nil, "app") {
		t.Fatal("nil UI must default to both model and app visible")
	}
	ui := &ToolUI{Visibility: []string{"app"}}
	if ToolUIVisibleTo(ui, "model") {
		t.Fatal("explicit app-only must exclude model")
	}
}

// ── §2 能力声明（两纪元均带 ui 扩展）────────────────────────────────────────

func TestClientCapabilities_DeclaresUIExtensionBothEras(t *testing.T) {
	c := &MCPClient{}
	caps := c.clientCapabilities()
	exts, _ := caps["extensions"].(map[string]any)
	if _, ok := exts[extensionUI]; !ok {
		t.Fatalf("clientCapabilities (shared by both eras) must declare ui extension: %v", caps)
	}
	merged := c.withMeta(map[string]any{}, "2026-07-28")
	meta, _ := merged["_meta"].(map[string]any)
	mergedCaps, _ := meta[metaClientCapabilities].(map[string]any)
	mergedExts, _ := mergedCaps["extensions"].(map[string]any)
	if _, ok := mergedExts[extensionUI]; !ok {
		t.Fatalf("withMeta must not drop ui extension when merging tasks: %v", mergedExts)
	}
	if _, ok := mergedExts[extensionTasks]; !ok {
		t.Fatalf("withMeta must still declare tasks extension (new era): %v", mergedExts)
	}
}

// ── §1 可见性：app-only 不进模型目录但可经 CallToolAsApp 调用；跨服务器/model-only 拒绝 ──

type fakeCatalog struct {
	entries map[string]protocol.CatalogEntry
}

func newFakeCatalog() *fakeCatalog                                                   { return &fakeCatalog{entries: map[string]protocol.CatalogEntry{}} }
func (f *fakeCatalog) List(context.Context, types.TrustTier) []protocol.CatalogEntry { return nil }
func (f *fakeCatalog) Lookup(name string) (protocol.CatalogEntry, bool) {
	e, ok := f.entries[name]
	return e, ok
}
func (f *fakeCatalog) Register(e protocol.CatalogEntry)                            { f.entries[e.Name] = e }
func (f *fakeCatalog) Unregister(name string)                                      { delete(f.entries, name) }
func (f *fakeCatalog) Invalidate()                                                 {}
func (f *fakeCatalog) Schemas(context.Context, types.TrustTier) []types.ToolSchema { return nil }

// newAppsTestManager 构造一个可真正执行 InProcess 工具（经 ExecEnvelope）的
// MCPManager：MCP 工具的 AssignSandboxTier 恒为 InProcess（assign.go），
// 因此 router 无需 Container/Wasm/Remote 即可完整路由。
func newAppsTestManager(t *testing.T) (*MCPManager, *fakeCatalog) {
	mgr, cat, _ := newAppsTestManagerWithRegistry(t)
	return mgr, cat
}

// newAppsTestManagerWithRegistry 装配真实 InMemoryToolRegistry：View 调用必须走注册表同一执行体。
func newAppsTestManagerWithRegistry(t *testing.T) (*MCPManager, *fakeCatalog, *tool.InMemoryToolRegistry) {
	t.Helper()
	inProc := sandbox.NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	router := sandbox.NewSandboxRouter(inProc, nil, nil, runtime.GOOS, 2)
	env := sandbox.NewExecEnvelope(&mockPolicyGate{}, router, 2, runtime.GOOS, nil)

	mgr := NewMCPManager(inProc, testSafeHTTP(nil), &mockPolicyGate{})
	mgr.SetEnvelope(env)
	cat := newFakeCatalog()
	mgr.SetCatalog(cat)
	reg := tool.NewInMemoryToolRegistry(env, config.DefaultThresholds().M7Tool)
	mgr.SetToolRegistrar(reg)
	return mgr, cat, reg
}

func scriptedToolCallServer(t *testing.T) *scriptedServer {
	t.Helper()
	return newScripted(func(method string, _ map[string]any, _ http.Header) (int, http.Header, string) {
		if method == methodToolsCall {
			return 200, nil, `{"result":{"content":[{"type":"text","text":"ok"}],"structuredContent":{"x":1}}}`
		}
		return 200, nil, `{"result":{}}`
	})
}

func TestVisibility_AppOnlyExcludedFromModelCatalogButCallable(t *testing.T) {
	mgr, cat, reg := newAppsTestManagerWithRegistry(t)
	srv := scriptedToolCallServer(t)
	client := NewMCPClient(MCPClientConfig{ServerName: "srv1", Transport: MCPStreamableHTTP, URL: "https://mcp.example/mcp", Trusted: true, TrustTier: 3}, testSafeHTTP(srv))

	tools := []MCPTool{
		{Name: "widget_refresh", Description: "app-only", InputSchema: []byte(`{"type":"object"}`), UI: &ToolUI{Visibility: []string{"app"}}},
		{Name: "get_weather", Description: "default visibility", InputSchema: []byte(`{"type":"object"}`)},
		{Name: "secret_model_only", Description: "model-only", InputSchema: []byte(`{"type":"object"}`), UI: &ToolUI{Visibility: []string{"model"}}},
	}
	valid := mgr.registerTools("srv1", "srv1", client, tools)
	if len(valid) != 3 {
		t.Fatalf("expected 3 valid tools, got %d", len(valid))
	}
	mgr.mu.Lock()
	mgr.entries["srv1"] = &mcpEntry{name: "srv1", tools: valid, client: client, cfg: MCPClientConfig{TrustTier: 3, Trusted: true}}
	mgr.entries["srv2-empty"] = &mcpEntry{name: "srv2", tools: nil, client: client, cfg: MCPClientConfig{TrustTier: 3}}
	mgr.mu.Unlock()

	// app-only 不进模型工具目录（Catalog）
	if _, ok := cat.Lookup(MCPToolName("srv1", "widget_refresh")); ok {
		t.Fatal("app-only tool must not be registered into model catalog")
	}
	// 默认可见性 + model-only 都在目录里
	if _, ok := cat.Lookup(MCPToolName("srv1", "get_weather")); !ok {
		t.Fatal("default-visibility tool must be in model catalog")
	}
	if _, ok := cat.Lookup(MCPToolName("srv1", "secret_model_only")); !ok {
		t.Fatal("model-only tool must still be in model catalog")
	}

	ctx := context.Background()
	// 模型路径（ExecuteTool）调不到 app-only 工具，也在注册表列表里看不到
	if _, err := reg.ExecuteTool(ctx, MCPToolName("srv1", "widget_refresh"), []byte(`{}`), types.TaintLow); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("model path must not reach app-only tool, got %v", err)
	}
	for _, rt := range reg.List() {
		if rt.Name == MCPToolName("srv1", "widget_refresh") {
			t.Fatal("app-only tool must not appear in registry List")
		}
	}
	// app-only 工具可经 CallToolAsApp 调用
	raw, err := mgr.CallToolAsApp(ctx, "srv1", "widget_refresh", nil, "sess-1", "view-1")
	if err != nil {
		t.Fatalf("app-only tool call via CallToolAsApp must succeed: %v", err)
	}
	var parsed struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || len(parsed.Content) != 1 {
		t.Fatalf("raw CallToolResult must be passed through: %s (%v)", raw, err)
	}
	// 默认可见性工具（含 "app"）同样可调用
	if _, err := mgr.CallToolAsApp(ctx, "srv1", "get_weather", nil, "sess-1", "view-1"); err != nil {
		t.Fatalf("default-visibility tool must be callable by app: %v", err)
	}
	// model-only 工具被 view 调用必须拒绝
	if _, err := mgr.CallToolAsApp(ctx, "srv1", "secret_model_only", nil, "sess-1", "view-1"); apperr.CodeOf(err) != apperr.CodeForbidden {
		t.Fatalf("model-only tool call by app must be Forbidden, got %v", err)
	}
	// 跨服务器：srv2-empty 没有 widget_refresh，即便 srv1 有同名工具也必须拒绝
	if _, err := mgr.CallToolAsApp(ctx, "srv2-empty", "widget_refresh", nil, "sess-1", "view-1"); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("cross-server tool call must be rejected as NotFound, got %v", err)
	}
	// 未连接的服务器
	if _, err := mgr.CallToolAsApp(ctx, "does-not-exist", "widget_refresh", nil, "", ""); apperr.CodeOf(err) != apperr.CodeNotFound {
		t.Fatalf("unknown server must be NotFound, got %v", err)
	}
}

// ── §1 ReadUIResource：text/blob、错误 mime、CSP 过滤、openai 别名、缓存 ──────

func newUIResourceManager(t *testing.T, handler func(method string, params map[string]any, h http.Header) (int, http.Header, string)) (*MCPManager, *scriptedServer) {
	t.Helper()
	mgr, _ := newAppsTestManager(t)
	srv := newScripted(handler)
	client := NewMCPClient(MCPClientConfig{ServerName: "srv1", Transport: MCPStreamableHTTP, URL: "https://mcp.example/mcp"}, testSafeHTTP(srv))
	mgr.mu.Lock()
	mgr.entries["srv1"] = &mcpEntry{name: "srv1", client: client}
	mgr.mu.Unlock()
	return mgr, srv
}

func TestReadUIResource_TextContent(t *testing.T) {
	mgr, _ := newUIResourceManager(t, func(string, map[string]any, http.Header) (int, http.Header, string) {
		return 200, nil, `{"result":{"contents":[{"uri":"ui://srv1/dash","mimeType":"text/html;profile=mcp-app","text":"<html>hi</html>"}]}}`
	})
	res, err := mgr.ReadUIResource(context.Background(), "srv1", "ui://srv1/dash")
	if err != nil || res.HTML != "<html>hi</html>" {
		t.Fatalf("text content: %+v %v", res, err)
	}
}

func TestReadUIResource_BlobContent(t *testing.T) {
	// base64("<html>b</html>") = "PGh0bWw+YjwvaHRtbD4="
	mgr, _ := newUIResourceManager(t, func(string, map[string]any, http.Header) (int, http.Header, string) {
		return 200, nil, `{"result":{"contents":[{"uri":"ui://srv1/dash","mimeType":"text/html+skybridge","blob":"PGh0bWw+YjwvaHRtbD4="}]}}`
	})
	res, err := mgr.ReadUIResource(context.Background(), "srv1", "ui://srv1/dash")
	if err != nil || res.HTML != "<html>b</html>" {
		t.Fatalf("blob content: %+v %v", res, err)
	}
}

func TestReadUIResource_RejectsBadMimeType(t *testing.T) {
	mgr, _ := newUIResourceManager(t, func(string, map[string]any, http.Header) (int, http.Header, string) {
		return 200, nil, `{"result":{"contents":[{"uri":"ui://srv1/dash","mimeType":"text/plain","text":"nope"}]}}`
	})
	if _, err := mgr.ReadUIResource(context.Background(), "srv1", "ui://srv1/dash"); apperr.CodeOf(err) != apperr.CodeInvalidInput {
		t.Fatalf("bad mime type must be rejected, got %v", err)
	}
}

func TestReadUIResource_RejectsNonUIScheme(t *testing.T) {
	mgr, _ := newUIResourceManager(t, func(string, map[string]any, http.Header) (int, http.Header, string) {
		return 200, nil, `{"result":{"contents":[]}}`
	})
	if _, err := mgr.ReadUIResource(context.Background(), "srv1", "not-ui-scheme"); apperr.CodeOf(err) != apperr.CodeInvalidInput {
		t.Fatalf("non ui:// uri must be rejected before any RPC, got %v", err)
	}
}

func TestReadUIResource_CSPHttpsOnlyFiltering(t *testing.T) {
	mgr, _ := newUIResourceManager(t, func(string, map[string]any, http.Header) (int, http.Header, string) {
		return 200, nil, `{"result":{"contents":[{"uri":"ui://srv1/dash","mimeType":"text/html;profile=mcp-app","text":"<html/>",
			"_meta":{"ui":{"csp":{"connectDomains":["https://good.example","http://insecure.example","javascript:alert(1)","https://*.wild.example"]}}}}]}}`
	})
	res, err := mgr.ReadUIResource(context.Background(), "srv1", "ui://srv1/dash")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.CSP.ConnectDomains) != 2 || res.CSP.ConnectDomains[0] != "https://good.example" || res.CSP.ConnectDomains[1] != "https://*.wild.example" {
		t.Fatalf("non-https / non-origin domains must be dropped: %+v", res.CSP.ConnectDomains)
	}
}

func TestReadUIResource_OpenAIWidgetCSPAlias(t *testing.T) {
	mgr, _ := newUIResourceManager(t, func(string, map[string]any, http.Header) (int, http.Header, string) {
		return 200, nil, `{"result":{"contents":[{"uri":"ui://srv1/dash","mimeType":"text/html;profile=mcp-app","text":"<html/>",
			"_meta":{"openai/widgetCSP":{"connect_domains":["https://api.example"],"resource_domains":["https://cdn.example"]}}}]}}`
	})
	res, err := mgr.ReadUIResource(context.Background(), "srv1", "ui://srv1/dash")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.CSP.ConnectDomains) != 1 || res.CSP.ConnectDomains[0] != "https://api.example" ||
		len(res.CSP.ResourceDomains) != 1 || res.CSP.ResourceDomains[0] != "https://cdn.example" {
		t.Fatalf("openai/widgetCSP alias must map to UICSP: %+v", res.CSP)
	}
}

func TestReadUIResource_CacheAndInvalidation(t *testing.T) {
	var calls atomic.Int64
	mgr, _ := newUIResourceManager(t, func(method string, _ map[string]any, _ http.Header) (int, http.Header, string) {
		if method == "resources/read" {
			calls.Add(1)
		}
		return 200, nil, `{"result":{"contents":[{"uri":"ui://srv1/dash","mimeType":"text/html;profile=mcp-app","text":"<html/>"}]}}`
	})
	ctx := context.Background()
	if _, err := mgr.ReadUIResource(ctx, "srv1", "ui://srv1/dash"); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.ReadUIResource(ctx, "srv1", "ui://srv1/dash"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("second read must be served from cache, got %d RPCs", calls.Load())
	}
	mgr.clearUIResourceCache("srv1")
	if _, err := mgr.ReadUIResource(ctx, "srv1", "ui://srv1/dash"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("cache invalidation must force a fresh read, got %d RPCs", calls.Load())
	}
}
