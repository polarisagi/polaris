package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	webui "github.com/polarisagi/polaris/web"
)

// ============================================================================
// MCP Apps Sandbox proxy 监听器（M8f-1，apps_spec.mdx "Sandbox proxy" 第 1 条：
// 宿主页面与 Sandbox proxy 必须异源）。网关在主监听器之外启动第二个 http.Server，
// 只托管 GET /sandbox.html（内容来自 web 嵌入文件，占位实现，8f-2 替换为真正的
// 双 iframe 代理页）。不挂鉴权中间件——页面本身不含任何数据，只转发宿主
// postMessage，异源本身即是这里的安全边界（CSP frame-ancestors 限制谁能嵌入它）。
// ============================================================================

// MCPAppsSandboxConfig 沙箱监听器配置（由 InterfaceConfig 推导，见 NewMCPAppsSandboxConfig）。
type MCPAppsSandboxConfig struct {
	Enabled bool
	Host    string
	// Port 监听端口；0 = 由操作系统分配（实际端口在 Start 后经 BoundPort 取得）。
	Port int
	// MainPort 主监听器端口（frame-ancestors 用）；0 = 未知，按端口通配。
	MainPort int
	// SandboxOrigin 显式配置的对外沙箱源（反向代理部署）；空 = 前端按自身 hostname + 沙箱端口推导。
	SandboxOrigin string
	// HostOrigin 显式配置的宿主页面对外源（反向代理部署时加入 frame-ancestors）。
	HostOrigin string
}

// NewMCPAppsSandboxConfig 从 InterfaceConfig 计算沙箱监听器配置。
//
// 为什么不在服务端推导完整沙箱源：用户可能经 localhost、局域网 IP 或域名访问同一实例，服务端
// 在启动时无从得知浏览器用的是哪个主机名。此前按监听地址推导，0.0.0.0 监听时固定为 127.0.0.1，
// 局域网访问的浏览器既连不上沙箱、宿主页也不在 frame-ancestors 内。现由前端用自身 hostname +
// 沙箱端口拼出沙箱源，frame-ancestors 按沙箱页请求的 hostname 逐请求生成。
func NewMCPAppsSandboxConfig(iface config.InterfaceConfig) MCPAppsSandboxConfig {
	port := iface.AppsSandboxPort
	if port == 0 && iface.Port != 0 {
		port = iface.Port + 1
	}
	return MCPAppsSandboxConfig{
		Enabled:       iface.AppsEnabled,
		Host:          iface.Host,
		Port:          port,
		MainPort:      iface.Port,
		SandboxOrigin: iface.AppsSandboxOrigin,
		HostOrigin:    iface.AppsHostOrigin,
	}
}

// frameAncestors 沙箱页 CSP frame-ancestors：浏览器访问沙箱页所用 hostname 上的主端口（http/https
// 两种 scheme；回环同时接受 localhost/127.0.0.1 两种写法），再加显式配置的宿主源。Host 头由请求方
// 提供，但伪造它只会改变伪造者自己拿到的响应，不影响其他用户。
func (c MCPAppsSandboxConfig) frameAncestors(requestHost string) string {
	hostname := requestHost
	if h, _, err := net.SplitHostPort(requestHost); err == nil {
		hostname = h
	}
	port := "*"
	if c.MainPort != 0 {
		port = strconv.Itoa(c.MainPort)
	}
	names := []string{hostname}
	if hostname == "127.0.0.1" || hostname == "localhost" {
		names = []string{"127.0.0.1", "localhost"}
	}
	var out []string
	for _, n := range names {
		hp := net.JoinHostPort(n, port)
		out = append(out, "http://"+hp, "https://"+hp)
	}
	if c.HostOrigin != "" {
		out = append(out, c.HostOrigin)
	}
	return strings.Join(out, " ")
}

// SetMCPAppsSandboxConfig 注入沙箱监听器配置；必须在 Start() 之前调用。
func (s *Server) SetMCPAppsSandboxConfig(cfg MCPAppsSandboxConfig) {
	s.appsSandboxCfg = cfg
}

// MCPAppsSandboxConfig 返回当前沙箱监听器配置（GET /v1/mcp-apps/config 读取）。
func (s *Server) MCPAppsSandboxConfig() MCPAppsSandboxConfig {
	return s.appsSandboxCfg
}

// startAppsSandboxListener 启动沙箱监听器（Start() 内调用，AppsEnabled=false 时
// 调用方跳过）。独立 *http.Server + 独立端口，与主监听器同一 SafeGo 启停范式
// （server_lifecycle.go Start/Shutdown）。
func (s *Server) startAppsSandboxListener() error {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sandbox.html", s.handleMCPAppsSandboxPage)

	addr := net.JoinHostPort(s.appsSandboxCfg.Host, strconv.Itoa(s.appsSandboxCfg.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mcp apps: sandbox listener failed to bind "+addr, err)
	}
	s.appsSandboxSrv = &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	if tcp, ok := ln.Addr().(*net.TCPAddr); ok {
		s.appsSandboxBoundPort.Store(int32(tcp.Port)) //nolint:gosec // TCP 端口范围在 int32 内
	}
	slog.Info("mcp apps: sandbox listener starting", "addr", ln.Addr().String())
	concurrent.SafeGo(context.Background(), "gateway.server.apps_sandbox_serve", func(context.Context) {
		if err := s.appsSandboxSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Error("mcp apps: sandbox listener serve error", "err", err)
		}
	})
	return nil
}

// handleMCPAppsSandboxPage GET /sandbox.html：唯一路由，其它路径由 ServeMux 默认
// 404。不挂鉴权中间件（apps_spec.mdx 未要求，页面本身不含数据）。
func (s *Server) handleMCPAppsSandboxPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// 不设 X-Frame-Options：本页需被宿主页面嵌入，CSP frame-ancestors 是这里
	// 唯一且正确的嵌入控制机制（两者同时存在时旧版浏览器可能优先 X-Frame-Options
	// 拒绝合法嵌入）。
	w.Header().Set("Content-Security-Policy", "frame-ancestors "+s.appsSandboxCfg.frameAncestors(r.Host))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write([]byte(webui.MCPAppsSandboxHTML)); err != nil {
		// 客户端提前断开等场景下写入失败无补救手段（HE-1 L4：记录后放弃），
		// 与本文件其余响应写入一致，不静默丢弃（inv_HE1_NoSilentErrorDiscard）。
		slog.Warn("mcp apps: sandbox page write failed", "err", err)
	}
}
