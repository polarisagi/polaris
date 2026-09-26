package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// mrtrServer 是按方法名 + 出现序号（同方法第几次调用）应答的 Streamable HTTP 假服务器，
// 记录每次调用的 JSON-RPC id（scriptedServer 按方法名覆盖存储，无法区分同一方法内的
// 多次调用，这里需要区分第一次/第二次 tools/call 才能验证 MRTR 的多轮往返）。
type mrtrServer struct {
	mu      sync.Mutex
	calls   map[string]int
	ids     map[string][]int64
	respond func(method string, seq int, id int64, params map[string]any) string
}

func newMRTRServer(respond func(method string, seq int, id int64, params map[string]any) string) *mrtrServer {
	return &mrtrServer{calls: map[string]int{}, ids: map[string][]int64{}, respond: respond}
}

func (s *mrtrServer) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	var rpc struct {
		ID     *int64         `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	_ = json.Unmarshal(body, &rpc)

	s.mu.Lock()
	s.calls[rpc.Method]++
	seq := s.calls[rpc.Method]
	var id int64
	if rpc.ID != nil {
		id = *rpc.ID
		s.ids[rpc.Method] = append(s.ids[rpc.Method], id)
	}
	s.mu.Unlock()

	payload := s.respond(rpc.Method, seq, id, rpc.Params)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(payload)),
		Request:    req,
	}, nil
}

func (s *mrtrServer) callCount(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[method]
}

func (s *mrtrServer) callIDs(method string) []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.ids[method]...)
}

func rpcResultEnvelope(id int64, result string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, id, result)
}

func rpcErrorEnvelope(id int64, code int, msg string) string {
	b, _ := json.Marshal(msg)
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"error":{"code":%d,"message":%s}}`, id, code, b)
}

// stubElicitor 记录收到的每个 ElicitRequest，固定应答 result（或 err）。
type stubElicitor struct {
	mu     sync.Mutex
	calls  []ElicitRequest
	result ElicitResult
	err    error
}

func (s *stubElicitor) Elicit(_ context.Context, req ElicitRequest) (ElicitResult, error) {
	s.mu.Lock()
	s.calls = append(s.calls, req)
	s.mu.Unlock()
	return s.result, s.err
}

func mrtrClient(srv *mrtrServer) *MCPClient {
	c := NewMCPClient(MCPClientConfig{ServerName: "srv", Transport: MCPStreamableHTTP, URL: "https://mcp.example/mcp"}, testSafeHTTP(srv))
	c.era.Store(int32(eraModern))
	return c
}

// TestMRTR_ElicitationRoundTrip 覆盖 tools/call 第一次返回 input_required（含
// elicitation/create 表单请求 + requestState），客户端调用统一输入处理器（经
// MCPManager.makeInputHandler → Elicitor）得到 inputResponses，连同原样回传的
// requestState 重试；第二次校验收到的内容后返回 complete。断言两次 JSON-RPC id
// 不同、Elicitor 收到正确的 ElicitRequest。
func TestMRTR_ElicitationRoundTrip(t *testing.T) {
	stub := &stubElicitor{result: ElicitResult{Action: "accept", Content: map[string]any{"name": "octocat"}}}
	mgr := NewMCPManager(nil, testSafeHTTP(nil), &mockPolicyGate{})
	mgr.SetElicitor(stub)
	handler := mgr.makeInputHandler("srv-1", "srv", 3)

	const schemaJSON = `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`

	srv := newMRTRServer(func(method string, seq int, id int64, params map[string]any) string {
		switch method {
		case "tools/call":
			if seq == 1 {
				return rpcResultEnvelope(id, `{"resultType":"input_required","inputRequests":{"github_login":{"method":"elicitation/create","params":{"mode":"form","message":"Please provide your GitHub username","requestedSchema":`+schemaJSON+`}}},"requestState":"opaque-blob"}`)
			}
			ir, _ := params["inputResponses"].(map[string]any)
			login, _ := ir["github_login"].(map[string]any)
			content, _ := login["content"].(map[string]any)
			if login["action"] != "accept" || content["name"] != "octocat" {
				t.Errorf("retry must carry the elicitor's answer, got inputResponses=%v", ir)
			}
			if params["requestState"] != "opaque-blob" {
				t.Errorf("requestState must be echoed verbatim, got %v", params["requestState"])
			}
			return rpcResultEnvelope(id, `{"resultType":"complete","content":[{"type":"text","text":"ok"}],"isError":false}`)
		default:
			return rpcResultEnvelope(id, `{"resultType":"complete"}`)
		}
	})

	c := mrtrClient(srv)
	c.SetInputHandler(handler, false, true)

	text, _, err := c.CallTool(context.Background(), "do_thing", map[string]any{"x": 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "ok" {
		t.Fatalf("expected ok, got %q", text)
	}

	ids := srv.callIDs("tools/call")
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("expected two distinct JSON-RPC ids for the initial call and the retry, got %v", ids)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.calls) != 1 {
		t.Fatalf("expected the elicitor to be invoked exactly once, got %d", len(stub.calls))
	}
	got := stub.calls[0]
	if got.ServerID != "srv-1" || got.ServerName != "srv" || got.Mode != "form" ||
		got.Message != "Please provide your GitHub username" || len(got.RequestedSchema) == 0 {
		t.Fatalf("unexpected ElicitRequest: %+v", got)
	}
}

// TestMRTR_ExceedsMaxRounds 服务器永远回答 input_required（只带 requestState，无新
// inputRequests）：客户端必须在 maxMRTRRounds 轮后放弃并报错，而不是无限重试。
func TestMRTR_ExceedsMaxRounds(t *testing.T) {
	srv := newMRTRServer(func(method string, _ int, id int64, _ map[string]any) string {
		if method == "tools/call" {
			return rpcResultEnvelope(id, `{"resultType":"input_required","requestState":"still-blocked"}`)
		}
		return rpcResultEnvelope(id, `{"resultType":"complete"}`)
	})
	c := mrtrClient(srv)

	_, err := c.request(context.Background(), "tools/call", map[string]any{"name": "do_thing"})
	if err == nil {
		t.Fatal("expected an error once MRTR rounds are exhausted")
	}
	if !apperr.IsCode(err, apperr.CodeResourceExhausted) {
		t.Fatalf("expected CodeResourceExhausted, got %v", err)
	}
	if got := srv.callCount("tools/call"); got != maxMRTRRounds {
		t.Fatalf("expected exactly %d attempts, got %d", maxMRTRRounds, got)
	}
}

// TestMRTR_UndeclaredCapability 服务器在 inputRequests 里请求 elicitation/create，
// 但客户端从未通过 SetInputHandler 声明 elicitation 能力：必须直接报错，不得重试
// （mrtr §Server Requirements 7 的客户端侧防御）。
func TestMRTR_UndeclaredCapability(t *testing.T) {
	srv := newMRTRServer(func(method string, _ int, id int64, _ map[string]any) string {
		if method == "tools/call" {
			return rpcResultEnvelope(id, `{"resultType":"input_required","inputRequests":{"x":{"method":"elicitation/create","params":{"message":"hi"}}},"requestState":"s"}`)
		}
		return rpcResultEnvelope(id, `{"resultType":"complete"}`)
	})
	c := mrtrClient(srv) // 未调用 SetInputHandler：hasElicitation 保持 false

	_, err := c.request(context.Background(), "tools/call", map[string]any{"name": "do_thing"})
	if err == nil {
		t.Fatal("expected an error for an undeclared capability request")
	}
	if !apperr.IsCode(err, apperr.CodeForbidden) {
		t.Fatalf("expected CodeForbidden, got %v", err)
	}
	if got := srv.callCount("tools/call"); got != 1 {
		t.Fatalf("expected exactly 1 attempt (no retry), got %d", got)
	}
}

// TestMRTR_HeaderMismatchRetry tools/call 收到 errCodeHeaderMismatch 时必须先
// ListTools 刷新 toolHeaders 缓存，再重试一次（2026-07-28 streamable-http SHOULD）。
func TestMRTR_HeaderMismatchRetry(t *testing.T) {
	srv := newMRTRServer(func(method string, seq int, id int64, _ map[string]any) string {
		switch method {
		case "tools/call":
			if seq == 1 {
				return rpcErrorEnvelope(id, errCodeHeaderMismatch, "header mismatch")
			}
			return rpcResultEnvelope(id, `{"content":[{"type":"text","text":"ok"}],"isError":false}`)
		case "tools/list":
			return rpcResultEnvelope(id, `{"tools":[]}`)
		default:
			return rpcResultEnvelope(id, `{}`)
		}
	})
	// 旧纪元客户端（未 Store eraModern）：HeaderMismatch 重试与协议纪元无关。
	c := NewMCPClient(MCPClientConfig{ServerName: "srv", Transport: MCPStreamableHTTP, URL: "https://mcp.example/mcp"}, testSafeHTTP(srv))

	text, _, err := c.CallTool(context.Background(), "do_thing", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "ok" {
		t.Fatalf("expected ok, got %q", text)
	}
	if got := srv.callCount("tools/list"); got != 1 {
		t.Fatalf("expected exactly one ListTools refresh, got %d", got)
	}
	if got := srv.callCount("tools/call"); got != 2 {
		t.Fatalf("expected exactly 2 tools/call attempts, got %d", got)
	}
}

// TestElicitationFormSchema_NestedObjectRejected 表单模式 requestedSchema 只允许平铺
// primitive 属性；嵌套 object 属于规范明确排除的复杂结构，必须被拒绝。
func TestElicitationFormSchema_NestedObjectRejected(t *testing.T) {
	raw := json.RawMessage(`{"mode":"form","message":"hi","requestedSchema":{"type":"object","properties":{
	  "nested":{"type":"object","properties":{"a":{"type":"string"}}}}}}`)
	if _, err := parseElicitRequest(context.Background(), "srv-1", "srv", raw); err == nil {
		t.Fatal("expected a nested object property to be rejected")
	}
}

// TestElicitationURL_NonHTTPSRejected url 模式必须是 https URL（elicitation §Safe URL
// Handling：SHOULD use HTTPS；本客户端收紧为 MUST，拒绝把明文链接展示给用户点击）。
func TestElicitationURL_NonHTTPSRejected(t *testing.T) {
	raw := json.RawMessage(`{"mode":"url","message":"go here","url":"http://example.com/login"}`)
	if _, err := parseElicitRequest(context.Background(), "srv-1", "srv", raw); err == nil {
		t.Fatal("expected a non-https url to be rejected")
	}
}

func TestInputHandler_RootsListAnswersEmpty(t *testing.T) {
	m := &MCPManager{}
	out, err := m.makeInputHandler("s1", "srv", 2)(context.Background(), "roots/list", 0, nil)
	if err != nil || string(out) != `{"roots":[]}` {
		t.Fatalf("roots/list: %s %v", out, err)
	}
}
