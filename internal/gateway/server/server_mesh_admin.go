package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/gateway/mesh"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// ============================================================================
// 跨节点委派的本机管理接口（ADR-0111 决策七第 4 条：两两配对）。
// 均在 /v1 下：对端令牌访问会被路由白名单 403，只有本机令牌 / POLARIS_API_KEY 可用。
// CLI 入口见 cmd/polaris/cli_peer.go。
// ============================================================================

// meshAdmin 取委派服务；未启用时写 404 并返回 nil。
func (s *Server) meshAdmin(w http.ResponseWriter) *mesh.Service {
	st := s.mesh.Load()
	if st == nil || !s.a2aConf().Enabled {
		httputil.RespondError(w, "Cross-node delegation is disabled", nil, http.StatusNotFound)
		return nil
	}
	return st.svc
}

// selfURLs 本节点可被其他节点访问的地址（advertised_url 优先，其次 extra_listen）。
func (s *Server) selfURLs() []string {
	var out []string
	if s.a2aConf().AdvertisedURL != "" {
		out = append(out, strings.TrimRight(s.a2aConf().AdvertisedURL, "/"))
	}
	for _, addr := range s.currentExtraListen() {
		out = append(out, "http://"+addr)
	}
	return out
}

// handleMeshSelf GET /v1/mesh/self —— 本节点身份（配对时给人核对指纹）。
func (s *Server) handleMeshSelf(w http.ResponseWriter, r *http.Request) {
	svc := s.meshAdmin(w)
	if svc == nil {
		return
	}
	self := svc.Self()
	httputil.WriteJSON(w, map[string]any{
		"name": s.a2aConf().Name, "node_id": self.NodeID, "fingerprint": self.Fingerprint(),
		"public_key": self.PublicKeyB64(), "urls": s.selfURLs(),
		"peer_read_only": svc.Policy().ReadOnly, "allow_relay": svc.Policy().AllowRelay,
	})
}

type inviteRequest struct {
	PeerName string `json:"peer_name"`
	URL      string `json:"url"`
	TTLDays  int    `json:"ttl_days"`
}

// issueInvite 签发邀请：令牌绑定到已知的对端节点 ID，未知时绑定占位 pending:<name>（对方 accept 后再绑定）。
func (s *Server) issueInvite(ctx context.Context, svc *mesh.Service, peerName, selfURL string, ttl time.Duration) (*mesh.Invite, string, error) {
	if err := mesh.ValidPeerName(peerName); err != nil {
		return nil, "", apperr.Wrap(apperr.CodeInvalidInput, "server: invite", err)
	}
	if selfURL == "" {
		if urls := s.selfURLs(); len(urls) > 0 {
			selfURL = urls[0]
		}
	}
	if selfURL == "" {
		return nil, "", apperr.New(apperr.CodeInvalidInput,
			"this node has no address other nodes can reach: set interface.extra_listen (e.g. your Tailscale or LAN IP:port) or pass --url")
	}
	nodeID := mesh.PendingNodeID(peerName)
	if p, err := svc.Peers().Get(ctx, peerName); err == nil {
		nodeID = p.NodeID
	}
	token, info, err := svc.Tokens().Issue(ctx, peerName, nodeID, nil, ttl)
	if err != nil {
		return nil, "", apperr.Wrap(apperr.CodeOf(err), "server: issue peer token", err)
	}
	// 新邀请作废此前未被接受的邀请令牌（已绑定节点的令牌仍可能在用，不动）。
	if n, serr := svc.Tokens().Supersede(ctx, peerName, info.TokenID, true); serr != nil {
		slog.Warn("mesh: revoke superseded pending tokens failed", "peer", peerName, "err", serr)
	} else if n > 0 {
		slog.Info("mesh: superseded pending invite tokens revoked", "peer", peerName, "count", n)
	}
	if p, err := svc.Peers().Get(ctx, peerName); err == nil {
		p.IssuedTokenID = info.TokenID
		if uerr := svc.Peers().Upsert(ctx, *p); uerr != nil {
			slog.Warn("mesh: record issued token on peer failed", "peer", peerName, "err", uerr)
		}
	}
	self := svc.Self()
	inv := &mesh.Invite{Version: 1, Name: s.a2aConf().Name, NodeID: self.NodeID, PublicKey: self.PublicKeyB64(),
		URL: strings.TrimRight(selfURL, "/"), Token: token}
	if !strings.HasPrefix(nodeID, "pending:") {
		inv.ForNodeID = nodeID
	}
	return inv, info.TokenID, nil
}

// handleMeshCreateInvite POST /v1/mesh/invites —— 第一步：为对方签发邀请。
func (s *Server) handleMeshCreateInvite(w http.ResponseWriter, r *http.Request) {
	svc := s.meshAdmin(w)
	if svc == nil {
		return
	}
	var req inviteRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		httputil.RespondError(w, "invalid request body", err, http.StatusBadRequest)
		return
	}
	inv, tokenID, err := s.issueInvite(r.Context(), svc, req.PeerName, req.URL, time.Duration(req.TTLDays)*24*time.Hour)
	if err != nil {
		writeAppErr(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{"invite": inv.Encode(), "token_id": tokenID,
		"node_id": svc.Self().NodeID, "fingerprint": svc.Self().Fingerprint(), "url": inv.URL})
}

type acceptRequest struct {
	Name   string `json:"name"`
	Invite string `json:"invite"`
	URL    string `json:"url"` // 回邀请时本节点的地址（可选）
}

// handleMeshAcceptPeer POST /v1/mesh/peers —— 第二步：接受对方邀请。
//  1. 校验邀请（节点 ID 由公钥派生、令牌是否发给本节点）；
//  2. 登记对端（SafeDialer 才会放行它的 host:port），拉取其 Agent Card 并用邀请里的公钥验签——
//     邀请由人带过来，人核对过指纹，这就是「固定公钥」的来源；
//  3. 把先前为它预签发的 pending 令牌绑定到真实节点 ID；若还没有为它签发过令牌，生成回邀请；
//  4. 返回 MCP Server 配置，由调用方（CLI）经 /v1/mcp-servers 走既有安装与授权链路登记。
func (s *Server) handleMeshAcceptPeer(w http.ResponseWriter, r *http.Request) {
	svc := s.meshAdmin(w)
	if svc == nil {
		return
	}
	var req acceptRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		httputil.RespondError(w, "invalid request body", err, http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	if err := mesh.ValidPeerName(req.Name); err != nil {
		writeAppErr(w, err)
		return
	}
	inv, err := mesh.DecodeInvite(req.Invite)
	if err != nil {
		writeAppErr(w, err)
		return
	}
	self := svc.Self()
	if inv.NodeID == self.NodeID {
		writeAppErr(w, apperr.New(apperr.CodeInvalidInput, "this invite was created by this node itself"))
		return
	}
	if inv.ForNodeID != "" && inv.ForNodeID != self.NodeID {
		writeAppErr(w, apperr.New(apperr.CodeForbidden, "this invite was issued to node "+inv.ForNodeID+", not to this node"))
		return
	}
	peer := mesh.Peer{Name: req.Name, NodeID: inv.NodeID, URL: inv.URL, PublicKey: inv.PublicKey}
	if err := svc.Peers().Upsert(ctx, peer); err != nil {
		writeAppErr(w, err)
		return
	}
	pub, _ := peer.PublicKeyBytes()
	if _, err := mesh.GetVerifiedCard(ctx, s.httpClient, inv.URL, pub); err != nil {
		if rerr := svc.Peers().Remove(ctx, req.Name); rerr != nil {
			slog.Warn("mesh: rollback peer registration failed", "peer", req.Name, "err", rerr)
		}
		writeAppErr(w, err)
		return
	}
	svc.Peers().MarkSeen(ctx, req.Name, "")

	resp := map[string]any{
		"peer": peer, "fingerprint": inv.Fingerprint(), "remote_name": inv.Name,
		"mcp_server": map[string]any{
			"id": mesh.MCPServerID(req.Name), "name": req.Name, "transport": "streamable_http",
			"url": inv.URL + "/mcp", "headers": map[string]string{"Authorization": "Bearer " + inv.Token},
			// trust_tier 2（Community）：默认 auto_review 模式下无需额外审批即可登记——`peer accept`
			// 本身就是人对该对端的显式确认；结果仍按 TrustTier<=2 打 TaintHigh。HTTP 传输不进 bwrap，
			// requires_network 对它无意义，置 false 免得出现「待网络审批」的误导告警。
			"enabled": true, "timeout": 30, "trust_tier": 2, "requires_network": false,
		},
	}
	bound, err := s.finishPairing(ctx, svc, req, peer, inv, resp)
	if err != nil {
		writeAppErr(w, err)
		return
	}
	slog.Info("mesh: peer accepted", "peer", req.Name, "node_id", inv.NodeID, "url", inv.URL, "paired", bound)
	httputil.WriteJSON(w, resp)
}

// finishPairing accept 的收尾：双向已完成则作废旧令牌并标记 paired，否则签发回邀请写入 resp。
func (s *Server) finishPairing(ctx context.Context, svc *mesh.Service, req acceptRequest, peer mesh.Peer,
	inv *mesh.Invite, resp map[string]any) (bool, error) {
	tokenID, bound, err := s.completedPairingToken(ctx, svc, req.Name, inv)
	if err != nil {
		return false, err
	}
	if !bound {
		reply, _, rerr := s.issueInvite(ctx, svc, req.Name, req.URL, 0)
		if rerr != nil {
			return false, rerr
		}
		resp["reply_invite"] = reply.Encode()
		return false, nil
	}
	// 对方已用这枚令牌完成 accept（它的 MCP 连接配置已换成新令牌），之前签发给它的都作废。
	if n, serr := svc.Tokens().Supersede(ctx, req.Name, tokenID, false); serr != nil {
		slog.Warn("mesh: revoke superseded peer tokens failed", "peer", req.Name, "err", serr)
	} else if n > 0 {
		slog.Info("mesh: superseded peer tokens revoked", "peer", req.Name, "count", n)
	}
	peer.IssuedTokenID = tokenID
	if uerr := svc.Peers().Upsert(ctx, peer); uerr != nil {
		slog.Warn("mesh: record bound token on peer failed", "peer", req.Name, "err", uerr)
	}
	resp["paired"] = true // 双向都已完成
	return true, nil
}

// completedPairingToken 判断这次 accept 之后双向配对是否已完成，返回对方手里那枚本节点令牌。
//   - 首次配对：本节点先前发出的 pending 邀请令牌被绑定到对方真实节点 ID；
//   - 重新配对：对方的邀请已写明发给本节点（for_node_id，说明它登记过本节点，也就持有本节点的令牌），
//     且本节点仍有签发给它、未吊销未过期的令牌——取最新一枚。不这样判断时，两边都已互相登记的
//     重新配对会无限互发回邀请（双实例实测发现）。
func (s *Server) completedPairingToken(ctx context.Context, svc *mesh.Service, name string, inv *mesh.Invite) (string, bool, error) {
	tokenID, bound, err := svc.Tokens().BindPendingToken(ctx, name, inv.NodeID)
	if err != nil {
		return "", false, apperr.Wrap(apperr.CodeOf(err), "server: bind pending peer token", err)
	}
	if bound {
		return tokenID, true, nil
	}
	if inv.ForNodeID == "" {
		return "", false, nil
	}
	tokens, err := svc.Tokens().List(ctx) // 按签发时间倒序
	if err != nil {
		return "", false, apperr.Wrap(apperr.CodeOf(err), "server: list peer tokens", err)
	}
	now := time.Now()
	for _, t := range tokens {
		if t.PeerName == name && t.PeerNodeID == inv.NodeID && !t.Revoked && now.Before(t.ExpiresAt) {
			return t.TokenID, true, nil
		}
	}
	return "", false, nil
}

// handleMeshListPeers GET /v1/mesh/peers[?probe=1] —— 对端列表；probe=1 时并发拉卡验签探活（5s 超时）。
func (s *Server) handleMeshListPeers(w http.ResponseWriter, r *http.Request) {
	svc := s.meshAdmin(w)
	if svc == nil {
		return
	}
	peers, err := svc.Peers().List(r.Context())
	if err != nil {
		writeAppErr(w, err)
		return
	}
	if r.URL.Query().Get("probe") == "1" {
		s.probePeers(r.Context(), svc, peers)
		if peers, err = svc.Peers().List(r.Context()); err != nil {
			writeAppErr(w, err)
			return
		}
	}
	connected := s.meshMCPConnected()
	out := make([]map[string]any, 0, len(peers))
	for _, p := range peers {
		out = append(out, map[string]any{"name": p.Name, "node_id": p.NodeID, "url": p.URL,
			"last_seen_at": p.LastSeenAt, "last_error": p.LastError, "mcp_connected": connected[mesh.MCPServerID(p.Name)]})
	}
	httputil.WriteJSON(w, map[string]any{"peers": out})
}

// handleMeshRemovePeer DELETE /v1/mesh/peers/{name} —— 解除登记并吊销本节点签发给它的令牌。
func (s *Server) handleMeshRemovePeer(w http.ResponseWriter, r *http.Request) {
	svc := s.meshAdmin(w)
	if svc == nil {
		return
	}
	ctx := r.Context()
	name := r.PathValue("name")
	p, err := svc.Peers().Get(ctx, name)
	if err != nil {
		writeAppErr(w, err)
		return
	}
	revoked := 0
	tokens, err := svc.Tokens().List(ctx)
	if err != nil {
		writeAppErr(w, err)
		return
	}
	for _, t := range tokens {
		if !t.Revoked && (t.PeerNodeID == p.NodeID || t.PeerNodeID == mesh.PendingNodeID(name)) {
			if err := svc.Tokens().Revoke(ctx, t.TokenID); err == nil {
				revoked++
			}
		}
	}
	if err := svc.Peers().Remove(ctx, name); err != nil {
		writeAppErr(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{"removed": name, "tokens_revoked": revoked, "mcp_server_id": mesh.MCPServerID(name)})
}

// handleMeshListTokens GET /v1/mesh/tokens。
func (s *Server) handleMeshListTokens(w http.ResponseWriter, r *http.Request) {
	svc := s.meshAdmin(w)
	if svc == nil {
		return
	}
	tokens, err := svc.Tokens().List(r.Context())
	if err != nil {
		writeAppErr(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{"tokens": tokens})
}

// handleMeshRevokeToken DELETE /v1/mesh/tokens/{tokenID}。
func (s *Server) handleMeshRevokeToken(w http.ResponseWriter, r *http.Request) {
	svc := s.meshAdmin(w)
	if svc == nil {
		return
	}
	if err := svc.Tokens().Revoke(r.Context(), r.PathValue("tokenID")); err != nil {
		writeAppErr(w, err)
		return
	}
	httputil.WriteJSON(w, map[string]any{"revoked": r.PathValue("tokenID")})
}

// meshMCPConnected 各对端 MCP 连接状态（按 server ID）。
func (s *Server) meshMCPConnected() map[string]bool {
	out := map[string]bool{}
	if s.mcpMgr == nil {
		return out
	}
	for _, info := range s.mcpMgr.ListServers() {
		out[info.ID] = info.Connected
	}
	return out
}

// probePeers 并发拉卡验签，结果写回 last_seen_at / last_error。
func (s *Server) probePeers(ctx context.Context, svc *mesh.Service, peers []mesh.Peer) map[string]bool {
	var mu sync.Mutex
	ok := map[string]bool{}
	var wg sync.WaitGroup
	for _, p := range peers {
		pub, err := p.PublicKeyBytes()
		if err != nil {
			continue
		}
		wg.Add(1)
		concurrent.SafeGo(ctx, "mesh.probe_peer", func(ctx context.Context) {
			defer wg.Done()
			_, ferr := mesh.GetVerifiedCard(ctx, s.httpClient, p.URL, pub)
			msg := ""
			if ferr != nil {
				msg = "Peer unavailable"
			}
			svc.Peers().MarkSeen(ctx, p.Name, msg)
			mu.Lock()
			ok[p.Name] = ferr == nil
			mu.Unlock()
		})
	}
	wg.Wait()
	return ok
}

// runMeshReconnect 对端休眠醒来后自动恢复 MCP 连接（ADR-0111 决策七）：启动时离线的对端，
// MCP 连接建立失败后不会自行重试；这里每分钟探活一次，可达且未连接的重新 StartFromDB。
func (s *Server) runMeshReconnect(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		st := s.mesh.Load()
		if st == nil || s.mcpMgr == nil {
			continue
		}
		peers, err := st.svc.Peers().List(ctx)
		if err != nil || len(peers) == 0 {
			continue
		}
		reachable := s.probePeers(ctx, st.svc, peers)
		connected := s.meshMCPConnected()
		for _, p := range peers {
			id := mesh.MCPServerID(p.Name)
			if !reachable[p.Name] || connected[id] {
				continue
			}
			if err := s.mcpMgr.StartFromDB(ctx, id); err != nil {
				// 对端已登记但 MCP Server 尚未经 CLI 登记（配对未完成）时会走到这里，每分钟一次，降为 Debug。
				slog.Debug("mesh: reconnect peer failed", "peer", p.Name, "err", err)
				continue
			}
			slog.Info("mesh: peer back online, MCP connection restored", "peer", p.Name)
		}
	}
}

// writeAppErr apperr → HTTP 状态码。
func writeAppErr(w http.ResponseWriter, err error) {
	code := apperr.CodeOf(err)
	httputil.RespondError(w, "", err, apperr.HTTPStatus(code))
}
