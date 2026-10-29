package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ============================================================================
// 设置页「多节点」：GET/PUT /v1/mesh/settings（ADR-0111）。
//
// 改写的是用户 config.toml（config.PatchUserConfig：只改目标行，注释与其余内容保留），
// 能热生效的立即生效——节点名（Agent Card 重挂）、额外监听地址（增量启停监听器）、对端任务
// 策略（mesh.Service.SetPolicy）；只有 a2a.enabled 的开关需要重启。
// ============================================================================

// meshSettingsBody 设置页表单（PUT 时全量提交）。
type meshSettingsBody struct {
	Enabled      bool     `json:"enabled"`
	NodeName     string   `json:"node_name"`
	ExtraListen  []string `json:"extra_listen"`
	PeerReadOnly bool     `json:"peer_read_only"`
	AllowRelay   bool     `json:"allow_relay"`
	PeerFileRoot string   `json:"peer_file_root"`
}

// listenSuggestion 本机可供其他节点访问的候选地址。
type listenSuggestion struct {
	Addr string `json:"addr"`
	Kind string `json:"kind"` // tailscale / lan
}

// handleGetMeshSettings GET /v1/mesh/settings —— a2a 关闭时同样可用（用来把它打开）。
func (s *Server) handleGetMeshSettings(w http.ResponseWriter, _ *http.Request) {
	httputil.WriteJSON(w, s.meshSettingsView(nil))
}

func (s *Server) meshSettingsView(extra map[string]any) map[string]any {
	a2a := s.a2aConf()
	body := meshSettingsBody{Enabled: a2a.Enabled, NodeName: a2a.Name, ExtraListen: s.currentExtraListen()}
	if body.ExtraListen == nil {
		body.ExtraListen = []string{}
	}
	if mc := s.meshCfg.Load(); mc != nil {
		body.PeerReadOnly, body.AllowRelay, body.PeerFileRoot = mc.PeerReadOnly, mc.AllowRelay, mc.PeerFileRoot
	}
	out := map[string]any{
		"settings": body, "config_path": s.configPath, "suggestions": s.listenSuggestions(),
		"running": false, "listening": body.ExtraListen,
	}
	if st := s.mesh.Load(); st != nil {
		pol := st.svc.Policy()
		body.PeerReadOnly, body.AllowRelay = pol.ReadOnly, pol.AllowRelay
		out["settings"] = body
		out["running"] = true
		out["effective_file_root"] = pol.FileRoot
		self := st.svc.Self()
		out["node_id"], out["fingerprint"] = self.NodeID, self.Fingerprint()
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// handlePutMeshSettings PUT /v1/mesh/settings —— 校验 → 试绑新监听地址 → 写 config.toml → 热生效。
func (s *Server) handlePutMeshSettings(w http.ResponseWriter, r *http.Request) {
	var req meshSettingsBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		httputil.RespondError(w, "invalid request body", err, http.StatusBadRequest)
		return
	}
	if err := s.validateMeshSettings(&req); err != nil {
		writeAppErr(w, err)
		return
	}
	if s.configPath == "" {
		writeAppErr(w, apperr.New(apperr.CodeInternal, "settings: config file path unknown, cannot persist"))
		return
	}
	if err := config.PatchUserConfig(s.configPath, []config.KeyPatch{
		{Section: "a2a", Key: "enabled", Value: req.Enabled},
		{Section: "a2a", Key: "name", Value: req.NodeName},
		{Section: "interface", Key: "extra_listen", Value: req.ExtraListen},
		{Section: "mesh", Key: "peer_read_only", Value: req.PeerReadOnly},
		{Section: "mesh", Key: "allow_relay", Value: req.AllowRelay},
		{Section: "mesh", Key: "peer_file_root", Value: req.PeerFileRoot},
	}); err != nil {
		writeAppErr(w, err)
		return
	}
	restart := s.applyMeshSettings(req)
	slog.Info("settings: mesh settings saved", "config", s.configPath, "node_name", req.NodeName,
		"extra_listen", req.ExtraListen, "restart_required", restart)
	httputil.WriteJSON(w, s.meshSettingsView(map[string]any{"restart_required": restart}))
}

// validateMeshSettings 规范化并校验表单；新增的监听地址先试绑一次，确认是本机地址且端口可用。
func (s *Server) validateMeshSettings(req *meshSettingsBody) error {
	req.NodeName = strings.TrimSpace(req.NodeName)
	if req.NodeName == "" || len(req.NodeName) > 64 || strings.IndexFunc(req.NodeName, unicode.IsControl) >= 0 {
		return apperr.New(apperr.CodeInvalidInput, "节点名称不能为空，最长 64 个字符")
	}
	seen := map[string]bool{}
	listen := make([]string, 0, len(req.ExtraListen))
	for _, a := range req.ExtraListen {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] {
			continue
		}
		seen[a] = true
		listen = append(listen, a)
	}
	req.ExtraListen = listen
	if err := checkListenAddrs(listen, s.currentExtraListen()); err != nil {
		return err
	}
	req.PeerFileRoot = strings.TrimSpace(req.PeerFileRoot)
	if req.PeerFileRoot != "" {
		if !filepath.IsAbs(req.PeerFileRoot) {
			return apperr.New(apperr.CodeInvalidInput, "共享目录必须是绝对路径")
		}
		if err := guard.CheckScopeRoot(filepath.Clean(req.PeerFileRoot)); err != nil {
			return apperr.Wrap(apperr.CodeInvalidInput, "共享目录不能是主目录、数据目录或其它敏感目录（及其上级）", err)
		}
	}
	return nil
}

// checkListenAddrs host:port 格式、不允许通配（与 config.validateListen 同口径，给出中文提示），
// 不在 bound 中的新地址试绑一次。
func checkListenAddrs(addrs, bound []string) error {
	already := map[string]bool{}
	for _, b := range bound {
		already[b] = true
	}
	for _, a := range addrs {
		host, port, err := net.SplitHostPort(a)
		if err != nil || port == "" {
			return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("监听地址 %q 格式应为 IP:端口，例如 100.101.102.103:28888", a))
		}
		ip := net.ParseIP(strings.Trim(host, "[]"))
		if ip == nil {
			return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("监听地址 %q 须写 IP，不能写主机名", a))
		}
		if ip.IsUnspecified() {
			return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("监听地址 %q 会对所有网卡开放，请写具体的 Tailscale 或局域网 IP", a))
		}
		if already[a] {
			continue
		}
		ln, err := net.Listen("tcp", a)
		if err != nil {
			return apperr.Wrap(apperr.CodeInvalidInput, fmt.Sprintf("无法监听 %s：不是本机地址，或端口已被占用", a), err)
		}
		if cerr := ln.Close(); cerr != nil {
			slog.Warn("settings: close probe listener failed", "addr", a, "err", cerr)
		}
	}
	return nil
}

// applyMeshSettings 热生效能生效的部分；返回是否还需要重启（仅 a2a.enabled 变化时）。
func (s *Server) applyMeshSettings(req meshSettingsBody) bool {
	prev := s.a2aConf()
	next := prev
	next.Name = req.NodeName
	restart := req.Enabled != prev.Enabled
	// enabled 的变化只写进配置、重启后生效：门面、令牌校验与对端任务执行角色都在启动时装配。
	s.a2aLive.Store(&next)

	for addr, err := range s.applyExtraListen(req.ExtraListen) {
		slog.Error("settings: extra listener failed after save", "addr", addr, "err", err)
	}
	mc := config.MeshConfig{}
	if cur := s.meshCfg.Load(); cur != nil {
		mc = *cur
	}
	mc.PeerReadOnly, mc.AllowRelay, mc.PeerFileRoot = req.PeerReadOnly, req.AllowRelay, req.PeerFileRoot
	s.meshCfg.Store(&mc)

	if st := s.mesh.Load(); st != nil {
		pol := st.svc.Policy()
		pol.ReadOnly, pol.AllowRelay = req.PeerReadOnly, req.AllowRelay
		pol.FileRoot = resolvePeerFileRoot(s.dataDir, req.PeerFileRoot)
		st.svc.SetPolicy(pol)
		if next.Name != prev.Name {
			s.mountMesh(st.svc, st.blockingWait) // Agent Card 带节点名，重挂门面路由
		}
	}
	return restart
}

// resolvePeerFileRoot 与 cmd/polaris/boot_mesh.go peerFileRoot 同一规则：空 = <data_dir>/workspace/peer-shared。
func resolvePeerFileRoot(dataDir, configured string) string {
	if configured != "" {
		return filepath.Clean(configured)
	}
	return filepath.Join(dataDir, "workspace", "peer-shared")
}

// listenSuggestions 枚举本机网卡上可给其他节点访问的地址：Tailscale（100.64.0.0/10）优先，
// 其次私网 IPv4。端口取本机主服务端口。只读网卡信息，不调用 tailscale CLI（ADR-0111 不做专用集成）。
func (s *Server) listenSuggestions() []listenSuggestion {
	port := s.mainPort()
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []listenSuggestion
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, aerr := ifc.Addrs()
		if aerr != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if kind := addrKind(ipn.IP); kind != "" {
					out = append(out, listenSuggestion{Addr: net.JoinHostPort(ipn.IP.String(), port), Kind: kind})
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Kind == "tailscale" && out[j].Kind != "tailscale" })
	return out
}

// addrKind IPv4 的候选类别：Tailscale CGNAT 段 / 私网；其余返回空。
func addrKind(ip net.IP) string {
	if ip.To4() == nil {
		return ""
	}
	_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
	switch {
	case cgnat.Contains(ip):
		return "tailscale"
	case ip.IsPrivate():
		return "lan"
	}
	return ""
}

// mainPort 本机主服务端口（候选监听地址沿用同一端口，方便记忆）。
func (s *Server) mainPort() string {
	for _, a := range []string{s.boundAddr, s.addr} {
		if _, p, err := net.SplitHostPort(a); err == nil && p != "" && p != "0" {
			return p
		}
	}
	return "28888"
}
