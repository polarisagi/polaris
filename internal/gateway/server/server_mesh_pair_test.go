package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/gateway/egress"
	"github.com/polarisagi/polaris/internal/gateway/mesh"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/security/network"
	storerepo "github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

// pairNode 一台「节点」：只装配配对与委派门面需要的部分，出站走带对端放行表的 SafeDialer
// （与生产 bootMesh 同一装配），以此同时验证 SafeDialer 只放行登记过的对端。
type pairNode struct {
	s   *Server
	svc *mesh.Service
	srv *httptest.Server
	bb  *pairBB
}

type pairBB struct {
	protocol.Blackboard
	posted []*types.TaskEntry
}

func (b *pairBB) PostTask(_ context.Context, e *types.TaskEntry) error {
	b.posted = append(b.posted, e)
	return nil
}

func (b *pairBB) PeekTask(_ context.Context, id string) (*types.TaskSnapshot, error) {
	return &types.TaskSnapshot{ID: id, Status: types.TaskPending}, nil
}

const pairLocalToken = "local-token-for-tests"

func newPairNode(t *testing.T, name string) *pairNode {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ddl, _ := schema.FS.ReadFile("048_mesh.sql")
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	self, err := mesh.GetOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bb := &pairBB{}
	svc := mesh.NewService(storerepo.NewSQLiteMeshRepository(db), bb, self, mesh.DefaultConfig())
	sd := network.NewSafeDialer(0, nil, config.M11PolicyThresholds{SafeDialerTOCTOUDelayMs: 1})
	sd.SetPeerAllowlist(svc.Peers().Allowed)

	n := &pairNode{svc: svc, bb: bb}
	// 与生产 boot_substrate 同一套出站栈：SafeDialer + EgressGateway 域名白名单，两层都挂对端放行。
	safe := network.NewSafeHTTPClient(sd)
	gw := egress.NewEgressGateway(safe.Transport, egress.DefaultAllowedDomains())
	gw.SetPeerAllowlist(svc.Peers().Allowed)
	n.s = &Server{a2aCfg: config.A2AConfig{Enabled: true, Name: name}, rootCtx: context.Background(),
		httpClient: &http.Client{Transport: gw}}
	n.s.SetLocalToken(pairLocalToken)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/agent-card.json", n.s.serveMesh)
	mux.HandleFunc("/a2a/v1", n.s.serveMesh)
	mux.HandleFunc("/mcp", n.s.serveMesh)
	mux.HandleFunc("GET /v1/mesh/self", n.s.handleMeshSelf)
	mux.HandleFunc("GET /v1/mesh/settings", n.s.handleGetMeshSettings)
	mux.HandleFunc("PUT /v1/mesh/settings", n.s.handlePutMeshSettings)
	mux.HandleFunc("POST /v1/mesh/invites", n.s.handleMeshCreateInvite)
	mux.HandleFunc("GET /v1/mesh/peers", n.s.handleMeshListPeers)
	mux.HandleFunc("POST /v1/mesh/peers", n.s.handleMeshAcceptPeer)
	mux.HandleFunc("DELETE /v1/mesh/peers/{name}", n.s.handleMeshRemovePeer)
	n.srv = httptest.NewServer(n.s.withMiddleware(mux))
	t.Cleanup(n.srv.Close)
	n.s.a2aCfg.AdvertisedURL = n.srv.URL
	n.s.SetMeshService(svc, config.MeshConfig{})
	n.s.isReady.Store(true)
	return n
}

func (n *pairNode) call(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, n.srv.URL+path, &buf)
	req.Header.Set("Authorization", "Bearer "+pairLocalToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestPairing_TwoNodesEndToEnd 两两配对闭环：A 邀请 → B 接受（验 A 的卡签名、回邀请）→ A 接受
// （绑定预签发令牌）。随后用 A 拿到的令牌、以 A 的节点身份经 MCP 门面向 B 委派：链尾与令牌绑定的
// 节点一致，被受理；伪造链尾被拒绝。
func TestPairing_TwoNodesEndToEnd(t *testing.T) {
	a, b := newPairNode(t, "laptop"), newPairNode(t, "desktop")

	code, inv := a.call(t, http.MethodPost, "/v1/mesh/invites", map[string]any{"peer_name": "desktop"})
	if code != http.StatusOK {
		t.Fatalf("invite: %d %v", code, inv)
	}
	code, accB := b.call(t, http.MethodPost, "/v1/mesh/peers", map[string]any{"name": "laptop", "invite": inv["invite"]})
	if code != http.StatusOK || accB["reply_invite"] == nil || accB["fingerprint"] != a.svc.Self().Fingerprint() {
		t.Fatalf("B accept: %d %v", code, accB)
	}
	code, accA := a.call(t, http.MethodPost, "/v1/mesh/peers", map[string]any{"name": "desktop", "invite": accB["reply_invite"]})
	if code != http.StatusOK || accA["paired"] != true {
		t.Fatalf("A accept must complete pairing: %d %v", code, accA)
	}

	// A 签发给 B 的令牌已绑定到 B 的真实节点 ID（不再是 pending 占位）。
	tokens, _ := a.svc.Tokens().List(context.Background())
	if len(tokens) != 1 || tokens[0].PeerNodeID != b.svc.Self().NodeID {
		t.Fatalf("pending token must be bound to the accepting node, got %+v", tokens)
	}

	// A → B：用 B 签发给 A 的令牌（A accept 返回的 mcp_server 头）经 Polaris MCP 客户端委派。
	headers := accA["mcp_server"].(map[string]any)["headers"].(map[string]any)
	client := mcp.NewMCPClient(mcp.MCPClientConfig{ServerName: "desktop", Transport: mcp.MCPStreamableHTTP,
		URL: b.srv.URL + "/mcp", Headers: map[string]string{"Authorization": headers["Authorization"].(string)},
		Timeout: 5 * time.Second}, network.NewLoopbackSafeHTTPClient(config.DefaultThresholds().M11Policy))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	// 工具级错误（isError）由 Polaris 客户端转成 error 返回。
	if _, _, err := client.CallTool(ctx, "get_task", map[string]any{"task_id": "x"}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("sanity: get_task on unknown id should report not found, got %v", err)
	}
	// 伪造链尾（声称来自另一个节点）→ 拒绝，且不投递。
	if _, _, err := client.CallTool(ctx, "a2a_delegate", map[string]any{"context_summary": "x", "spawn_depth": 1,
		"chain": []string{"pn-someone-else"}}); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("forged chain tail must be rejected, got %v", err)
	}
	if len(b.bb.posted) != 0 {
		t.Fatal("rejected delegation must not reach B's blackboard")
	}
}

func TestPairing_RejectsBadInvites(t *testing.T) {
	a, b, c := newPairNode(t, "laptop"), newPairNode(t, "desktop"), newPairNode(t, "vps")

	// 发给 C 的邀请（已绑定 C 的节点 ID）不能被 B 使用。
	_ = c.svc.Peers().Upsert(context.Background(), mesh.Peer{Name: "laptop", NodeID: a.svc.Self().NodeID,
		URL: a.srv.URL, PublicKey: a.svc.Self().PublicKeyB64()})
	_ = a.svc.Peers().Upsert(context.Background(), mesh.Peer{Name: "vps", NodeID: c.svc.Self().NodeID,
		URL: c.srv.URL, PublicKey: c.svc.Self().PublicKeyB64()})
	_, inv := a.call(t, http.MethodPost, "/v1/mesh/invites", map[string]any{"peer_name": "vps"})
	if code, out := b.call(t, http.MethodPost, "/v1/mesh/peers", map[string]any{"name": "laptop", "invite": inv["invite"]}); code != http.StatusForbidden {
		t.Fatalf("invite bound to another node must be refused, got %d %v", code, out)
	}

	// 公钥与节点 ID 不符的邀请（篡改）→ 400。
	_, inv2 := a.call(t, http.MethodPost, "/v1/mesh/invites", map[string]any{"peer_name": "desktop"})
	decoded, _ := mesh.DecodeInvite(inv2["invite"].(string))
	decoded.PublicKey = c.svc.Self().PublicKeyB64()
	if code, _ := b.call(t, http.MethodPost, "/v1/mesh/peers", map[string]any{"name": "laptop", "invite": decoded.Encode()}); code != http.StatusBadRequest {
		t.Fatalf("tampered invite must be refused, got %d", code)
	}

	// 地址指向一台用别的密钥签卡的节点（中间人 / 写错地址）→ 验签失败，登记被回滚。
	decoded2, _ := mesh.DecodeInvite(inv2["invite"].(string))
	decoded2.URL = c.srv.URL
	if code, _ := b.call(t, http.MethodPost, "/v1/mesh/peers", map[string]any{"name": "laptop", "invite": decoded2.Encode()}); code != http.StatusForbidden {
		t.Fatalf("card signed by an unexpected key must be refused, got %d", code)
	}
	if peers, _ := b.svc.Peers().List(context.Background()); len(peers) != 0 {
		t.Fatalf("failed accept must roll back the peer registration, got %+v", peers)
	}
}

// TestPairing_RepairCompletesInOneRound 已互相登记的两节点重新配对：一次 invite + accept 即完成，
// 不再互发回邀请；B 侧只剩一枚有效令牌；A 侧旧令牌在 B 第一次用上新令牌时作废。
func TestPairing_RepairCompletesInOneRound(t *testing.T) {
	a, b := newPairNode(t, "laptop"), newPairNode(t, "desktop")
	_, inv := a.call(t, http.MethodPost, "/v1/mesh/invites", map[string]any{"peer_name": "desktop"})
	_, accB := b.call(t, http.MethodPost, "/v1/mesh/peers", map[string]any{"name": "laptop", "invite": inv["invite"]})
	if code, accA := a.call(t, http.MethodPost, "/v1/mesh/peers", map[string]any{"name": "desktop", "invite": accB["reply_invite"]}); code != http.StatusOK || accA["paired"] != true {
		t.Fatalf("initial pairing: %d %v", code, accA)
	}

	_, inv2 := a.call(t, http.MethodPost, "/v1/mesh/invites", map[string]any{"peer_name": "desktop"})
	code, acc := b.call(t, http.MethodPost, "/v1/mesh/peers", map[string]any{"name": "laptop", "invite": inv2["invite"]})
	if code != http.StatusOK || acc["paired"] != true || acc["reply_invite"] != nil {
		t.Fatalf("re-pairing must complete without a reply invite, got %d %v", code, acc)
	}
	valid := func(n *pairNode) int {
		toks, err := n.svc.Tokens().List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		c := 0
		for _, tk := range toks {
			if !tk.Revoked {
				c++
			}
		}
		return c
	}
	if valid(b) != 1 {
		t.Fatalf("re-pairing must leave B with a single valid token, got %d", valid(b))
	}
	// A 无法知道 B 是否接受了新邀请，旧令牌先保留。
	if valid(a) != 2 {
		t.Fatalf("A keeps its old token until B is known to hold the new one, got %d", valid(a))
	}
	auth := acc["mcp_server"].(map[string]any)["headers"].(map[string]any)["Authorization"].(string)
	req, err := http.NewRequest(http.MethodPost, a.srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("B must reach A with the new token: %v %v", resp, err)
	}
	resp.Body.Close()
	if valid(a) != 1 {
		t.Fatalf("first use of the new token must retire the old one, got %d valid", valid(a))
	}
}
