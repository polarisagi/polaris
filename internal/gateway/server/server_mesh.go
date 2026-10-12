package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/gateway/mesh"
	"github.com/polarisagi/polaris/internal/gateway/server/meshapi"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// meshState 跨节点委派门面的运行期状态（ADR-0111）。路由在 NewServer 时已注册，
// 委派服务经 SetMeshService 晚于 NewServer 注入，故用原子指针在请求期读取。
type meshState struct {
	svc *mesh.Service
	mux *http.ServeMux
}

// SetMeshService 注入委派服务并挂载 Agent Card / A2A 1.0 / MCP 三个门面。
// a2a.enabled=false 时不应调用；即使调用，serveMesh 也按开关 404。
func (s *Server) SetMeshService(svc *mesh.Service, meshCfg config.MeshConfig) {
	if svc == nil {
		return
	}
	mux := http.NewServeMux()
	meshapi.Register(mux, meshapi.Deps{
		Service:      svc,
		A2A:          s.a2aCfg,
		BlockingWait: time.Duration(meshCfg.BlockingWaitSeconds) * time.Second,
	})
	s.mesh.Store(&meshState{svc: svc, mux: mux})
	rootCtx := s.rootCtx
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	concurrent.SafeGo(rootCtx, "mesh.peer_reconnect", s.runMeshReconnect)
}

// SetExtraListen 额外监听地址（须在 Start 之前调用；已经 config.Validate 校验）。
func (s *Server) SetExtraListen(addrs []string) {
	s.extraListen = append([]string(nil), addrs...)
}

// serveMesh 分发到委派门面。未启用 / 未注入时 404：不暴露「这里本可以有个端点」。
func (s *Server) serveMesh(w http.ResponseWriter, r *http.Request) {
	st := s.mesh.Load()
	if st == nil || !s.a2aCfg.Enabled {
		http.NotFound(w, r)
		return
	}
	st.mux.ServeHTTP(w, r)
}

// verifyPeerToken 校验对端令牌。委派服务未注入（a2a 关闭）时一律拒绝。
func (s *Server) verifyPeerToken(ctx context.Context, token string) (*mesh.Principal, error) {
	st := s.mesh.Load()
	if st == nil || !s.a2aCfg.Enabled {
		return nil, mesh.ErrTokenUnknown
	}
	p, err := st.svc.Tokens().Verify(ctx, token)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "server: verify peer token", err)
	}
	return p, nil
}

// startExtraListeners 绑定额外监听地址（ADR-0111 决策六）：同一个 http.Server 服务多个 listener，
// 鉴权与路由完全一致，Shutdown 时一并关闭。绑定失败只告警不阻断：本机访问不受影响，跨节点功能降级。
func (s *Server) startExtraListeners() {
	for _, extra := range s.extraListen {
		eln, err := net.Listen("tcp", extra)
		if err != nil {
			slog.Error("polaris-server: extra listener failed, other nodes cannot reach this address", "addr", extra, "err", err)
			continue
		}
		if !encryptedTransport(eln.Addr()) {
			// 本监听器是明文 HTTP：Tailscale 地址由 WireGuard 加密，局域网地址则令牌对同网段可见。
			slog.Warn("polaris-server: extra listener is plaintext HTTP on a non-Tailscale address; peer tokens are visible to "+
				"anyone sniffing this network — prefer the Tailscale address or put a TLS reverse proxy in front (a2a.advertised_url=https://…)",
				"addr", eln.Addr().String())
		}
		slog.Info("polaris-server: extra listener started", "addr", eln.Addr().String())
		concurrent.SafeGo(context.Background(), "gateway.server.http_serve_extra", func(context.Context) {
			if err := s.srv.Serve(eln); err != nil && err != http.ErrServerClosed {
				slog.Error("polaris-server: extra serve error", "addr", extra, "err", err)
			}
		})
	}
}

// encryptedTransport 地址是否落在链路自带加密的网段：回环（不出本机）或 Tailscale CGNAT 段
// 100.64.0.0/10（WireGuard）。只用于提示，不用于放行判定。
func encryptedTransport(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return false
	}
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	return tcp.IP.IsLoopback() || cgnat.Contains(tcp.IP)
}
