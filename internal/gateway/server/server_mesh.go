package server

import (
	"context"
	"errors"
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
	svc          *mesh.Service
	mux          *http.ServeMux
	blockingWait time.Duration
}

// SetMeshService 注入委派服务并挂载 Agent Card / A2A 1.0 / MCP 三个门面。
// a2a.enabled=false 时不应调用；即使调用，serveMesh 也按开关 404。
func (s *Server) SetMeshService(svc *mesh.Service, meshCfg config.MeshConfig) {
	if svc == nil {
		return
	}
	s.SetMeshConfig(meshCfg)
	s.mountMesh(svc, time.Duration(meshCfg.BlockingWaitSeconds)*time.Second)
	rootCtx := s.rootCtx
	if rootCtx == nil {
		rootCtx = context.Background()
	}
	concurrent.SafeGo(rootCtx, "mesh.peer_reconnect", s.runMeshReconnect)
}

// mountMesh 用当前 [a2a] 配置构建门面路由并原子替换（设置页改节点名后重挂，Agent Card 随之更新）。
func (s *Server) mountMesh(svc *mesh.Service, blockingWait time.Duration) {
	mux := http.NewServeMux()
	meshapi.Register(mux, meshapi.Deps{Service: svc, A2A: s.a2aConf(), BlockingWait: blockingWait})
	s.mesh.Store(&meshState{svc: svc, mux: mux, blockingWait: blockingWait})
}

// SetMeshConfig 记录启动时的 [mesh] 配置（a2a 关闭时也调用，设置页据此展示）。
func (s *Server) SetMeshConfig(c config.MeshConfig) {
	s.meshCfg.Store(&c)
}

// SetConfigPath 用户 config.toml 路径（须在 Start 之前调用）。
func (s *Server) SetConfigPath(path string) {
	s.configPath = path
}

// a2aConf 当前生效的 [a2a] 配置：设置页改写过则取改写后的值。
func (s *Server) a2aConf() config.A2AConfig {
	if c := s.a2aLive.Load(); c != nil {
		return *c
	}
	return s.a2aCfg
}

// SetExtraListen 额外监听地址（须在 Start 之前调用；已经 config.Validate 校验）。
func (s *Server) SetExtraListen(addrs []string) {
	s.extraListen = append([]string(nil), addrs...)
}

// serveMesh 分发到委派门面。未启用 / 未注入时 404：不暴露「这里本可以有个端点」。
func (s *Server) serveMesh(w http.ResponseWriter, r *http.Request) {
	st := s.mesh.Load()
	if st == nil || !s.a2aConf().Enabled {
		http.NotFound(w, r)
		return
	}
	st.mux.ServeHTTP(w, r)
}

// verifyPeerToken 校验对端令牌。委派服务未注入（a2a 关闭）时一律拒绝。
func (s *Server) verifyPeerToken(ctx context.Context, token string) (*mesh.Principal, error) {
	st := s.mesh.Load()
	if st == nil || !s.a2aConf().Enabled {
		return nil, mesh.ErrTokenUnknown
	}
	p, err := st.svc.Tokens().Verify(ctx, token)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeOf(err), "server: verify peer token", err)
	}
	return p, nil
}

// currentExtraListen 当前额外监听地址（设置页可在运行期改写，读写都经 extraMu）。
func (s *Server) currentExtraListen() []string {
	s.extraMu.Lock()
	defer s.extraMu.Unlock()
	return append([]string(nil), s.extraListen...)
}

// startExtraListeners 启动时绑定额外监听地址（ADR-0111 决策六）。
func (s *Server) startExtraListeners() {
	for addr, err := range s.applyExtraListen(s.currentExtraListen()) {
		slog.Error("polaris-server: extra listener failed, other nodes cannot reach this address", "addr", addr, "err", err)
	}
}

// applyExtraListen 把额外监听器调整为 want：新增的绑定、去掉的关闭（关闭只停止接受新连接，
// 进行中的请求照常完成），未变的不动。同一个 http.Server 服务所有 listener，鉴权与路由完全一致，
// Shutdown 时一并关闭。返回绑定失败的地址及原因；失败只影响该地址，本机访问不受影响。
func (s *Server) applyExtraListen(want []string) map[string]error {
	s.extraMu.Lock()
	defer s.extraMu.Unlock()
	if s.extraLns == nil {
		s.extraLns = map[string]net.Listener{}
	}
	keep := make(map[string]bool, len(want))
	for _, a := range want {
		keep[a] = true
	}
	for addr, ln := range s.extraLns {
		if !keep[addr] {
			if err := ln.Close(); err != nil {
				slog.Warn("polaris-server: close extra listener failed", "addr", addr, "err", err)
			}
			delete(s.extraLns, addr)
			slog.Info("polaris-server: extra listener stopped", "addr", addr)
		}
	}
	failed := map[string]error{}
	for _, addr := range want {
		if _, ok := s.extraLns[addr]; ok {
			continue
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			failed[addr] = err
			continue
		}
		if !encryptedTransport(ln.Addr()) {
			// 本监听器是明文 HTTP：Tailscale 地址由 WireGuard 加密，局域网地址则令牌对同网段可见。
			slog.Warn("polaris-server: extra listener is plaintext HTTP on a non-Tailscale address; peer tokens are visible to "+
				"anyone sniffing this network — prefer the Tailscale address or put a TLS reverse proxy in front (a2a.advertised_url=https://…)",
				"addr", ln.Addr().String())
		}
		s.extraLns[addr] = ln
		slog.Info("polaris-server: extra listener started", "addr", ln.Addr().String())
		concurrent.SafeGo(context.Background(), "gateway.server.http_serve_extra", func(context.Context) {
			if err := s.srv.Serve(ln); err != nil && err != http.ErrServerClosed && !errors.Is(err, net.ErrClosed) {
				slog.Error("polaris-server: extra serve error", "addr", addr, "err", err)
			}
		})
	}
	s.extraListen = append([]string(nil), want...)
	return failed
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
