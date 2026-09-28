package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/config"
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
