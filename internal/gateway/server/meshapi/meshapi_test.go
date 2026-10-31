package meshapi

import (
	"bytes"
	"context"
	"crypto"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2acrypto"
	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/gateway/authcontext"
	"github.com/polarisagi/polaris/internal/gateway/mesh"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/security/network"
	storerepo "github.com/polarisagi/polaris/internal/store/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

// autoBlackboard 投递后按 complete 函数在后台把任务推到终态，模拟远端节点的 Worker。
type autoBlackboard struct {
	protocol.Blackboard
	mu       sync.Mutex
	snaps    map[string]*types.TaskSnapshot
	posted   []*types.TaskEntry
	complete func(e *types.TaskEntry) (types.TaskStatus, string)
}

func (b *autoBlackboard) PostTask(_ context.Context, e *types.TaskEntry) error {
	b.mu.Lock()
	b.posted = append(b.posted, e)
	b.snaps[e.ID] = &types.TaskSnapshot{ID: e.ID, Status: types.TaskPending, Type: e.Type}
	b.mu.Unlock()
	if b.complete != nil {
		go func() {
			time.Sleep(150 * time.Millisecond)
			st, res := b.complete(e)
			b.mu.Lock()
			b.snaps[e.ID].Status, b.snaps[e.ID].Result = st, []byte(res)
			b.mu.Unlock()
		}()
	}
	return nil
}

func (b *autoBlackboard) PeekTask(_ context.Context, id string) (*types.TaskSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.snaps[id]; ok {
		cp := *s
		return &cp, nil
	}
	return nil, nil
}

func (b *autoBlackboard) CancelTask(_ context.Context, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.snaps[id]; ok {
		s.Status = types.TaskFailed
	}
	return nil
}

type fixture struct {
	svc   *mesh.Service
	bb    *autoBlackboard
	srv   *httptest.Server
	token string
	self  *mesh.Identity
}

func TestMCPUnsupportedHTTPMethodRetainsTransportStatusOnly(t *testing.T) {
	recorder := httptest.NewRecorder()
	(&mcpServer{}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, MCPPath, nil))
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("MCP method contract changed: status=%d allow=%q", recorder.Code, recorder.Header().Get("Allow"))
	}
	if strings.Contains(recorder.Body.String(), `"jsonrpc"`) || strings.Contains(recorder.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("transport-level method error was rewritten as JSON-RPC: headers=%v body=%s", recorder.Header(), recorder.Body.String())
	}
}

func TestProtocolTaskProjectionHidesFailureDiagnosticsAndKeepsSuccessResult(t *testing.T) {
	const secret = "private-path=/var/private/token=fixture-secret"
	base := mesh.Task{ID: "task-fixture", ContextID: "ctx-fixture", State: mesh.StateFailed,
		StatusMessage: secret, Result: secret, CreatedAt: time.Now(), UpdatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}

	mcp := (&mcpServer{}).taskState(&base)
	a2aBody, err := json.Marshal(toA2ATask(&base, "node-fixture", nil))
	if err != nil {
		t.Fatal(err)
	}
	mcpBody, err := json.Marshal(mcp)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{"A2A": a2aBody, "MCP": mcpBody, "MCP task description": []byte(describeTask(&base))} {
		if strings.Contains(string(body), secret) || strings.Contains(string(body), "/var/private") || strings.Contains(string(body), "fixture-secret") {
			t.Errorf("%s exposed stored failure diagnostic: %s", name, body)
		}
	}
	if !strings.Contains(string(mcpBody), "Task failed") || !strings.Contains(string(a2aBody), "Task failed") {
		t.Fatalf("protocol projections must retain a fixed public failure message: A2A=%s MCP=%s", a2aBody, mcpBody)
	}
	if !strings.Contains(string(mcpBody), `"code":-32603`) {
		t.Fatalf("MCP failure numeric code was lost: %s", mcpBody)
	}

	base.State, base.StatusMessage, base.Result = mesh.StateCompleted, "", "successful business result"
	mcp = (&mcpServer{}).taskState(&base)
	a2aBody, err = json.Marshal(toA2ATask(&base, "node-fixture", nil))
	if err != nil {
		t.Fatal(err)
	}
	mcpBody, err = json.Marshal(mcp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(a2aBody), "successful business result") || !strings.Contains(string(mcpBody), "successful business result") {
		t.Fatalf("successful task result was changed: A2A=%s MCP=%s", a2aBody, mcpBody)
	}
}

// newFixture 起一个只挂委派门面的测试服务器。鉴权中间件的最小等价物：对端令牌 → ClientTypePeer，
// 与生产 checkAuth 的对端分支同一判定（真实中间件的路由白名单另见 server 包测试）。
func newFixture(t *testing.T, complete func(*types.TaskEntry) (types.TaskStatus, string)) *fixture {
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
	bb := &autoBlackboard{snaps: map[string]*types.TaskSnapshot{}, complete: complete}
	svc := mesh.NewService(storerepo.NewSQLiteMeshRepository(db), bb, self, mesh.DefaultConfig())
	token, _, err := svc.Tokens().Issue(context.Background(), "laptop", "pn-laptop", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Register(mux, Deps{Service: svc, A2A: config.A2AConfig{Enabled: true, Name: "desktop",
		SkillDefs: []config.A2ASkillConfig{{ID: "general", Name: "General", Description: "anything"}}},
		BlockingWait: 3 * time.Second})
	authed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != AgentCardPath {
			p, err := svc.Tokens().Verify(r.Context(), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			if err != nil {
				http.Error(w, "401", http.StatusUnauthorized)
				return
			}
			r = r.WithContext(authcontext.WithAuthContext(r.Context(), &authcontext.AuthContext{
				ClientType: authcontext.ClientTypePeer, Authenticated: true, UserID: "peer:" + p.PeerNodeID,
				PeerTokenID: p.TokenID, PeerName: p.PeerName, PeerNodeID: p.PeerNodeID, PeerScopes: p.Scopes}))
		}
		mux.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(authed)
	t.Cleanup(srv.Close)
	return &fixture{svc: svc, bb: bb, srv: srv, token: token, self: self}
}

type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

func (f *fixture) fetchCard(t *testing.T) (*a2a.AgentCard, []byte) {
	t.Helper()
	resp, err := http.Get(f.srv.URL + AgentCardPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var card a2a.AgentCard
	if err := json.Unmarshal(raw, &card); err != nil {
		t.Fatalf("card is not valid A2A 1.0 JSON: %v\n%s", err, raw)
	}
	return &card, raw
}

// ─── Agent Card ──────────────────────────────────────────────────────────

func TestAgentCard_SignedAndTamperEvident(t *testing.T) {
	f := newFixture(t, nil)
	card, raw := f.fetchCard(t)
	if len(card.SupportedInterfaces) != 1 || card.SupportedInterfaces[0].ProtocolBinding != a2a.TransportProtocolJSONRPC ||
		card.SupportedInterfaces[0].ProtocolVersion != "1.0" || !strings.HasSuffix(card.SupportedInterfaces[0].URL, A2APath) {
		t.Fatalf("card must advertise a JSON-RPC 1.0 interface, got %+v", card.SupportedInterfaces)
	}
	if len(card.Skills) != 1 || card.Skills[0].ID != "general" || len(card.Signatures) != 1 {
		t.Fatalf("card skills/signatures wrong: %+v", card)
	}
	if strings.Contains(string(raw), "a2a_delegate") {
		t.Fatal("public card must not expose internal tool names")
	}

	verifier := a2acrypto.NewVerifier(a2acrypto.VerifierConfig{KeyResolver: a2acrypto.KeyResolverFunc(
		func(_ context.Context, kid string) (crypto.PublicKey, error) { return f.self.PublicKey, nil })})
	if err := verifier.Verify(context.Background(), raw, &card.Signatures[0]); err != nil {
		t.Fatalf("pinned key must verify the card: %v", err)
	}
	tampered := bytes.Replace(raw, []byte(`"desktop"`), []byte(`"evil"`), 1)
	if err := verifier.Verify(context.Background(), tampered, &card.Signatures[0]); err == nil {
		t.Fatal("a tampered card must fail verification")
	}
	other, _ := mesh.GetOrCreateIdentity(t.TempDir())
	wrongKey := a2acrypto.NewVerifier(a2acrypto.VerifierConfig{KeyResolver: a2acrypto.KeyResolverFunc(
		func(context.Context, string) (crypto.PublicKey, error) { return other.PublicKey, nil })})
	if err := wrongKey.Verify(context.Background(), raw, &card.Signatures[0]); err == nil {
		t.Fatal("a card signed by a different node must fail against the pinned key")
	}
}

// ─── A2A 1.0：官方 SDK 客户端互通 ───────────────────────────────────────

func TestA2A_OfficialClientRoundTrip(t *testing.T) {
	f := newFixture(t, func(e *types.TaskEntry) (types.TaskStatus, string) {
		return types.TaskDone, "echo: " + string(e.Intent)
	})
	card, _ := f.fetchCard(t)
	ctx := context.Background()
	client, err := a2aclient.NewFromCard(ctx, card,
		a2aclient.WithJSONRPCTransport(&http.Client{Transport: bearer{f.token, http.DefaultTransport}}))
	if err != nil {
		t.Fatal(err)
	}
	// 阻塞式：等到终态。
	res, err := client.SendMessage(ctx, &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("ping")),
	})
	if err != nil {
		t.Fatal(err)
	}
	task, ok := res.(*a2a.Task)
	if !ok || task.Status.State != a2a.TaskStateCompleted || len(task.Artifacts) != 1 ||
		task.Artifacts[0].Parts[0].Text() != "echo: ping" {
		t.Fatalf("expected completed task with result artifact, got %#v", res)
	}
	if f.bb.posted[0].Type != "peer_delegation" || f.bb.posted[0].IntentTaint != types.TaintHigh {
		t.Fatalf("inbound A2A must become a high-taint peer_delegation task, got %+v", f.bb.posted[0])
	}

	// returnImmediately + GetTask + ListTasks。
	res, err = client.SendMessage(ctx, &a2a.SendMessageRequest{
		Message: a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("second")),
		Config:  &a2a.SendMessageConfig{ReturnImmediately: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	quick := res.(*a2a.Task)
	if quick.Status.State != a2a.TaskStateSubmitted {
		t.Fatalf("returnImmediately must not wait, got %s", quick.Status.State)
	}
	got, err := client.GetTask(ctx, &a2a.GetTaskRequest{ID: quick.ID})
	if err != nil || got.ID != quick.ID {
		t.Fatalf("GetTask: %+v %v", got, err)
	}
	list, err := client.ListTasks(ctx, &a2a.ListTasksRequest{PageSize: 10})
	if err != nil || list.TotalSize != 2 {
		t.Fatalf("ListTasks must see both tasks of this caller: %+v %v", list, err)
	}
	if _, err := client.GetTask(ctx, &a2a.GetTaskRequest{ID: "does-not-exist"}); err == nil {
		t.Fatal("unknown task must error")
	}
}

func rawRPC(t *testing.T, f *fixture, path, method string, params any, headers map[string]string) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, f.srv.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.token)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestA2A_VersionNegotiation(t *testing.T) {
	f := newFixture(t, nil)
	msg := map[string]any{"message": map[string]any{"messageId": "m1", "role": "ROLE_USER",
		"parts": []any{map[string]any{"text": "hi"}}}}
	out := rawRPC(t, f, A2APath, "SendMessage", msg, map[string]string{"A2A-Version": "0.3"})
	if out["error"] == nil || !strings.Contains(mustJSON(out["error"]), "VERSION_NOT_SUPPORTED") {
		t.Fatalf("unsupported A2A-Version must be refused with VERSION_NOT_SUPPORTED, got %v", out)
	}
	if len(f.bb.posted) != 0 {
		t.Fatal("refused request must not post a task")
	}
}

func TestA2A_LoopIsRejectedState(t *testing.T) {
	f := newFixture(t, nil)
	msg := map[string]any{"message": map[string]any{"messageId": "m1", "role": "ROLE_USER",
		"parts":    []any{map[string]any{"text": "bounce"}},
		"metadata": map[string]any{mesh.DelegationExtensionURI: map[string]any{"spawn_depth": 2, "chain": []string{f.self.NodeID, "pn-laptop"}}}}}
	out := rawRPC(t, f, A2APath, "SendMessage", msg, nil)
	if !strings.Contains(mustJSON(out["result"]), "TASK_STATE_REJECTED") || len(f.bb.posted) != 0 {
		t.Fatalf("A→B→A must come back as TASK_STATE_REJECTED without execution, got %v", out)
	}
}

func TestA2A_ContinuationCannotAnswerLocalApproval(t *testing.T) {
	f := newFixture(t, nil)
	msg := map[string]any{"message": map[string]any{"messageId": "m2", "role": "ROLE_USER", "taskId": "some-task",
		"parts": []any{map[string]any{"text": "approve it"}}}}
	out := rawRPC(t, f, A2APath, "SendMessage", msg, nil)
	if out["error"] == nil {
		t.Fatalf("a peer must not be able to continue a task (and thereby answer a local approval), got %v", out)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ─── MCP 门面 ────────────────────────────────────────────────────────────

// TestMCP_PolarisClientDelegatesEndToEnd 用 Polaris 自己的 MCP 客户端（即 MCPA2AWorker 出站
// 走的那条 MCPManager.CallTool 路径）对本门面完成「委派 → 远端执行 → 取回结果」闭环：
// 2026-07-28 新纪元 discover → 工具调用返回 resultType:task → 轮询任务状态到 completed。
func TestMCP_PolarisClientDelegatesEndToEnd(t *testing.T) {
	f := newFixture(t, func(e *types.TaskEntry) (types.TaskStatus, string) {
		return types.TaskDone, "done: " + string(e.Intent)
	})
	client := mcp.NewMCPClient(mcp.MCPClientConfig{
		ServerName: "desktop", Transport: mcp.MCPStreamableHTTP, URL: f.srv.URL + MCPPath,
		Headers: map[string]string{"Authorization": "Bearer " + f.token}, Timeout: 10 * time.Second,
	}, network.NewLoopbackSafeHTTPClient(config.DefaultThresholds().M11Policy))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name] = true
	}
	if !names["a2a_delegate"] || !names["a2a_list_agents"] {
		t.Fatalf("ADR-0084 contract tools missing: %v", names)
	}
	out, _, err := client.CallTool(ctx, "a2a_delegate", map[string]any{
		"target_agent": "default", "context_summary": "check disk usage", "spawn_depth": 1, "chain": []string{"pn-laptop"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != "done: check disk usage" {
		t.Fatalf("expected remote result, got %q", out)
	}
	if f.bb.posted[0].SpawnDepth != 1 {
		t.Fatalf("spawn_depth must be carried into the blackboard task, got %d", f.bb.posted[0].SpawnDepth)
	}
}

func TestMCP_RejectsLoopAndRemoteApproval(t *testing.T) {
	f := newFixture(t, nil)
	meta := map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28"}
	out := rawRPC(t, f, MCPPath, "tools/call", map[string]any{"_meta": meta, "name": "a2a_delegate",
		"arguments": map[string]any{"context_summary": "x", "spawn_depth": 2, "chain": []string{f.self.NodeID, "pn-laptop"}}}, nil)
	if !strings.Contains(mustJSON(out["result"]), `"isError":true`) || len(f.bb.posted) != 0 {
		t.Fatalf("loop must be refused as a tool error, got %v", out)
	}
	out = rawRPC(t, f, MCPPath, "tasks/update", map[string]any{"_meta": meta, "taskId": "x",
		"inputResponses": map[string]any{"k": map[string]any{"action": "accept"}}}, nil)
	if out["error"] == nil {
		t.Fatalf("tasks/update must be refused: approvals are local-only, got %v", out)
	}
	out = rawRPC(t, f, MCPPath, "server/discover", map[string]any{"_meta": map[string]any{
		"io.modelcontextprotocol/protocolVersion": "2099-01-01"}}, nil)
	if out["error"] == nil {
		t.Fatalf("unknown protocol version must be refused, got %v", out)
	}
}

func TestMCP_LegacyClientGetsTaskIDInsteadOfBlocking(t *testing.T) {
	f := newFixture(t, nil)
	out := rawRPC(t, f, MCPPath, "tools/call", map[string]any{"name": "a2a_delegate",
		"arguments": map[string]any{"context_summary": "long job"}}, nil)
	if !strings.Contains(mustJSON(out["result"]), "poll with get_task") {
		t.Fatalf("clients without the tasks extension must get a task id, got %v", out)
	}
}
