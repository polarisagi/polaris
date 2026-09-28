package server

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/config"

	webui "github.com/polarisagi/polaris/web"
)

func TestNewMCPAppsSandboxConfig_Ports(t *testing.T) {
	cfg := NewMCPAppsSandboxConfig(config.InterfaceConfig{Host: "0.0.0.0", Port: 28888, AppsEnabled: true})
	if cfg.Port != 28889 || cfg.MainPort != 28888 || cfg.SandboxOrigin != "" {
		t.Fatalf("default: port=main+1, origin left to the browser: %+v", cfg)
	}
	if c := NewMCPAppsSandboxConfig(config.InterfaceConfig{Port: 0}); c.Port != 0 {
		t.Fatalf("main port 0 must leave sandbox port to the OS, got %d", c.Port)
	}
	c := NewMCPAppsSandboxConfig(config.InterfaceConfig{Port: 28888, AppsSandboxPort: 9999,
		AppsSandboxOrigin: "https://sandbox.example.com", AppsHostOrigin: "https://polaris.example.com"})
	if c.Port != 9999 || c.SandboxOrigin != "https://sandbox.example.com" || c.HostOrigin != "https://polaris.example.com" {
		t.Fatalf("explicit overrides must win: %+v", c)
	}
}

func TestMCPAppsSandboxConfig_FrameAncestorsFollowRequestHost(t *testing.T) {
	cfg := MCPAppsSandboxConfig{MainPort: 28888}
	if got := cfg.frameAncestors("127.0.0.1:28889"); got != "http://127.0.0.1:28888 https://127.0.0.1:28888 http://localhost:28888 https://localhost:28888" {
		t.Fatalf("loopback must accept both spellings: %q", got)
	}
	// 局域网访问：宿主页在 192.168.1.5:28888，沙箱页经 192.168.1.5:28889 请求
	if got := cfg.frameAncestors("192.168.1.5:28889"); got != "http://192.168.1.5:28888 https://192.168.1.5:28888" {
		t.Fatalf("LAN host must be allowed as embedder: %q", got)
	}
	proxied := MCPAppsSandboxConfig{HostOrigin: "https://polaris.example.com"}
	if got := proxied.frameAncestors("sandbox.example.com"); got != "http://sandbox.example.com:* https://sandbox.example.com:* https://polaris.example.com" {
		t.Fatalf("unknown main port wildcards, configured host origin appended: %q", got)
	}
}

func TestHandleMCPAppsSandboxPage_ServesOnlySandboxPathWithHeaders(t *testing.T) {
	s := &Server{appsSandboxCfg: MCPAppsSandboxConfig{MainPort: 28888}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sandbox.html", s.handleMCPAppsSandboxPage)

	req := httptest.NewRequest(http.MethodGet, "/sandbox.html", nil)
	req.Host = "localhost:28889"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control: %q", rec.Header().Get("Cache-Control"))
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("X-Content-Type-Options: %q", rec.Header().Get("X-Content-Type-Options"))
	}
	if rec.Header().Get("X-Frame-Options") != "" {
		t.Errorf("must not set X-Frame-Options (page needs to be embeddable): %q", rec.Header().Get("X-Frame-Options"))
	}
	if got := rec.Header().Get("Content-Security-Policy"); got != "frame-ancestors http://127.0.0.1:28888 https://127.0.0.1:28888 http://localhost:28888 https://localhost:28888" {
		t.Errorf("CSP: %q", got)
	}
	if rec.Body.Len() == 0 {
		t.Error("expected non-empty body")
	}

	// 其它路径必须 404（ServeMux 默认行为，未注册任何其它 route）。
	req2 := httptest.NewRequest(http.MethodGet, "/anything-else", nil)
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unmatched path, got %d", rec2.Code)
	}

	// 未挂鉴权中间件：无 Authorization/Cookie 请求同样 200（本用例即证明——
	// 上面的请求本就没有携带任何令牌）。
}

// TestHandleMCPAppsSandboxPage_BodyImplementsProxyProtocol 确认 8f-2 的真实双 iframe
// 代理实现（而非 8f-1 占位页）被正确嵌入并served：必须发送
// ui/notifications/sandbox-proxy-ready、必须校验 host= 参数与 document.referrer 的
// origin 一致、必须只信任 window.parent 且 origin 匹配的消息、必须处理
// sandbox-resource-ready 通知。字符串断言直接对被 served 的响应体做，避免与
// webui.MCPAppsSandboxHTML 实现细节（go:embed 路径）耦合。
func TestHandleMCPAppsSandboxPage_BodyImplementsProxyProtocol(t *testing.T) {
	s := &Server{appsSandboxCfg: MCPAppsSandboxConfig{MainPort: 28888}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sandbox.html", s.handleMCPAppsSandboxPage)

	req := httptest.NewRequest(http.MethodGet, "/sandbox.html", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()

	mustContain := []string{
		// 占位页（8f-1）不含这些标记；真实实现必须都在。
		"ui/notifications/sandbox-proxy-ready",    // 就绪通知：代理必须主动通知宿主
		"ui/notifications/sandbox-resource-ready", // 处理宿主下发的 HTML 负载
		"document.referrer",                       // host= 参数需与 referrer 的 origin 比对
		"event.origin !== hostOrigin",             // 宿主消息只信任匹配 origin 的来源
		"event.source !== window.parent",          // 宿主消息只信任 window.parent
		"window.openai",                           // window.openai 兼容垫片已注入
	}
	for _, s := range mustContain {
		if !strings.Contains(body, s) {
			t.Errorf("sandbox.html body missing expected marker %q", s)
		}
	}

	// 8f-1 占位页的注释标记不应再存在（证明确实被 8f-2 替换而非叠加）。
	if strings.Contains(body, "真正的双 iframe 沙箱代理逻辑") {
		t.Error("sandbox.html still contains the 8f-1 placeholder comment; expected the real proxy implementation")
	}
}

// TestStartAppsSandboxListener_ReportsBoundPort 配置端口为 0 时由系统分配，配置接口必须报告实际端口，
// 否则前端拼出的沙箱源不可达。
func TestStartAppsSandboxListener_ReportsBoundPort(t *testing.T) {
	s := &Server{appsSandboxCfg: MCPAppsSandboxConfig{Enabled: true, Host: "127.0.0.1"}}
	if err := s.startAppsSandboxListener(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.appsSandboxSrv.Close(); err != nil {
			t.Log(err)
		}
	})
	port := s.appsSandboxBoundPort.Load()
	if port <= 0 {
		t.Fatalf("bound port must be recorded, got %d", port)
	}
	rec := httptest.NewRecorder()
	s.handleGetMCPAppsConfig(rec, httptest.NewRequest(http.MethodGet, "/v1/mcp-apps/config", nil))
	if !strings.Contains(rec.Body.String(), fmt.Sprintf(`"sandbox_port":%d`, port)) {
		t.Fatalf("config endpoint must report bound port %d: %s", port, rec.Body.String())
	}
}

// TestMCPAppsSandboxHTML_CSPHashMatchesInlineScript 沙箱页 CSP 以 sha256 锁定唯一内联脚本；
// 改脚本忘了重算哈希时浏览器会静默拒绝执行，沙箱整体失效——由本测试兜住。
func TestMCPAppsSandboxHTML_CSPHashMatchesInlineScript(t *testing.T) {
	page := webui.MCPAppsSandboxHTML
	// 取第一个 <script> 到其后第一个 </script>：脚本体内以字符串形式含有注入 View 的
	// "<script>" 片段（闭合标签写作 <\/script>，不会提前截断）。
	open := strings.Index(page, "<script>")
	if open < 0 {
		t.Fatal("sandbox page must contain an inline script")
	}
	start := open + len("<script>")
	end := strings.Index(page[start:], "</script>")
	if end < 0 {
		t.Fatal("unterminated inline script")
	}
	script := page[start : start+end]
	declared := regexp.MustCompile(`'sha256-([A-Za-z0-9+/=]+)'`).FindStringSubmatch(page)
	if declared == nil {
		t.Fatal("sandbox page CSP must pin the inline script by sha256")
	}
	sum := sha256.Sum256([]byte(script))
	if got := base64.StdEncoding.EncodeToString(sum[:]); got != declared[1] {
		t.Fatalf("CSP hash stale: declared %s, script hashes to %s", declared[1], got)
	}
}
