package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// tasksClient 构造一个已固化为新纪元的 Streamable HTTP 假客户端，复用 mrtrServer
// （见 mcp_client_mrtr_test.go）区分同一方法名的第几次调用。
func tasksClient(srv *mrtrServer) *MCPClient {
	c := NewMCPClient(MCPClientConfig{ServerName: "srv", Transport: MCPStreamableHTTP, URL: "https://mcp.example/mcp"}, testSafeHTTP(srv))
	c.era.Store(int32(eraModern))
	return c
}

// taskCreated methodToolsCall 返回的 CreateTaskResult（seed 状态，status 固定 working）。
func taskCreated(pollMs int) string {
	return fmt.Sprintf(`{"resultType":"task","taskId":"t-1","status":"working",`+
		`"createdAt":"2020-01-01T00:00:00Z","lastUpdatedAt":"2020-01-01T00:00:00Z","ttlMs":null,"pollIntervalMs":%d}`, pollMs)
}

// TestTasks_FullLifecycle 覆盖 working → input_required（含 elicitation/create）→
// completed 的完整轮询：断言只发送一次 tasks/update 且 inputResponses 正确；同一
// inputRequests key 在后续轮询里重复出现时不再触发 tasks/update；最终 CallTool 拿到
// completed 的文本结果。
func TestTasks_FullLifecycle(t *testing.T) {
	stub := &stubElicitor{result: ElicitResult{Action: "accept", Content: map[string]any{"name": "octocat"}}}
	mgr := NewMCPManager(nil, testSafeHTTP(nil), &mockPolicyGate{})
	mgr.SetElicitor(stub)
	handler := mgr.makeInputHandler("srv-1", "srv", 3)

	const inputReqs = `{"github_login":{"method":"elicitation/create","params":{"mode":"form","message":"who are you",` +
		`"requestedSchema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}}}}`

	var mu sync.Mutex
	updateCalls := 0

	srv := newMRTRServer(func(method string, seq int, id int64, params map[string]any) string {
		switch method {
		case "tools/call":
			return rpcResultEnvelope(id, taskCreated(5))
		case "tasks/get":
			switch seq {
			case 1:
				return rpcResultEnvelope(id, `{"resultType":"complete","taskId":"t-1","status":"working",`+
					`"createdAt":"2020-01-01T00:00:00Z","lastUpdatedAt":"2020-01-01T00:00:00Z","ttlMs":null,"pollIntervalMs":5}`)
			case 2, 3:
				return rpcResultEnvelope(id, `{"resultType":"complete","taskId":"t-1","status":"input_required",`+
					`"createdAt":"2020-01-01T00:00:00Z","lastUpdatedAt":"2020-01-01T00:00:00Z","ttlMs":null,"pollIntervalMs":5,`+
					`"inputRequests":`+inputReqs+`}`)
			default:
				return rpcResultEnvelope(id, `{"resultType":"complete","taskId":"t-1","status":"completed",`+
					`"createdAt":"2020-01-01T00:00:00Z","lastUpdatedAt":"2020-01-01T00:00:00Z","ttlMs":null,`+
					`"result":{"content":[{"type":"text","text":"done"}],"isError":false}}`)
			}
		case "tasks/update":
			mu.Lock()
			updateCalls++
			mu.Unlock()
			if params["taskId"] != "t-1" {
				t.Errorf("tasks/update missing taskId, got %v", params)
			}
			ir, _ := params["inputResponses"].(map[string]any)
			login, _ := ir["github_login"].(map[string]any)
			content, _ := login["content"].(map[string]any)
			if login["action"] != "accept" || content["name"] != "octocat" {
				t.Errorf("tasks/update inputResponses wrong, got %v", ir)
			}
			return rpcResultEnvelope(id, `{"resultType":"complete","taskId":"t-1","status":"input_required",`+
				`"createdAt":"2020-01-01T00:00:00Z","lastUpdatedAt":"2020-01-01T00:00:00Z","ttlMs":null}`)
		default:
			return rpcResultEnvelope(id, `{"resultType":"complete"}`)
		}
	})

	c := tasksClient(srv)
	c.SetInputHandler(handler, false, true)

	text, _, err := c.CallTool(context.Background(), "do_thing", map[string]any{"x": 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "done" {
		t.Fatalf("expected done, got %q", text)
	}

	mu.Lock()
	defer mu.Unlock()
	if updateCalls != 1 {
		t.Fatalf("expected exactly one tasks/update call (dedup across polls), got %d", updateCalls)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if len(stub.calls) != 1 {
		t.Fatalf("expected the elicitor to be invoked exactly once, got %d", len(stub.calls))
	}
}

// TestTasks_FailedReturnsRPCError failed 状态的 error 字段必须能通过 errors.As 还原成
// *RPCError，且 code 与服务器给出的一致（tasks §Task Status：failed 携带 JSON-RPC 错误）。
func TestTasks_FailedReturnsRPCError(t *testing.T) {
	srv := newMRTRServer(func(method string, _ int, id int64, _ map[string]any) string {
		switch method {
		case "tools/call":
			return rpcResultEnvelope(id, taskCreated(5))
		case "tasks/get":
			return rpcResultEnvelope(id, `{"resultType":"complete","taskId":"t-1","status":"failed",`+
				`"createdAt":"2020-01-01T00:00:00Z","lastUpdatedAt":"2020-01-01T00:00:00Z","ttlMs":null,`+
				`"error":{"code":-32001,"message":"boom"}}`)
		default:
			return rpcResultEnvelope(id, `{"resultType":"complete"}`)
		}
	})
	c := tasksClient(srv)

	_, err := c.request(context.Background(), methodToolsCall, map[string]any{"name": "do_thing"})
	if err == nil {
		t.Fatal("expected an error for a failed task")
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected *RPCError in the error chain, got %v", err)
	}
	if rpcErr.Code != -32001 {
		t.Fatalf("expected code -32001, got %d", rpcErr.Code)
	}
}

// TestTasks_Cancelled cancelled 状态必须映射为 apperr.CodeCancelled。
func TestTasks_Cancelled(t *testing.T) {
	srv := newMRTRServer(func(method string, _ int, id int64, _ map[string]any) string {
		switch method {
		case "tools/call":
			return rpcResultEnvelope(id, taskCreated(5))
		case "tasks/get":
			return rpcResultEnvelope(id, `{"resultType":"complete","taskId":"t-1","status":"cancelled",`+
				`"createdAt":"2020-01-01T00:00:00Z","lastUpdatedAt":"2020-01-01T00:00:00Z","ttlMs":null}`)
		default:
			return rpcResultEnvelope(id, `{"resultType":"complete"}`)
		}
	})
	c := tasksClient(srv)

	_, err := c.request(context.Background(), methodToolsCall, map[string]any{"name": "do_thing"})
	if !apperr.IsCode(err, apperr.CodeCancelled) {
		t.Fatalf("expected CodeCancelled, got %v", err)
	}
}

// TestTasks_TTLExpired createdAt 已远超 ttlMs 且任务仍未到达终态时，必须以
// apperr.CodeTimeout 兜底退出，而不是无限轮询下去。
func TestTasks_TTLExpired(t *testing.T) {
	past := time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339)
	srv := newMRTRServer(func(method string, _ int, id int64, _ map[string]any) string {
		switch method {
		case "tools/call":
			return rpcResultEnvelope(id, fmt.Sprintf(`{"resultType":"task","taskId":"t-1","status":"working",`+
				`"createdAt":%q,"lastUpdatedAt":%q,"ttlMs":1000,"pollIntervalMs":5}`, past, past))
		case "tasks/get":
			return rpcResultEnvelope(id, fmt.Sprintf(`{"resultType":"complete","taskId":"t-1","status":"working",`+
				`"createdAt":%q,"lastUpdatedAt":%q,"ttlMs":1000,"pollIntervalMs":5}`, past, past))
		default:
			return rpcResultEnvelope(id, `{"resultType":"complete"}`)
		}
	})
	c := tasksClient(srv)

	_, err := c.request(context.Background(), methodToolsCall, map[string]any{"name": "do_thing"})
	if !apperr.IsCode(err, apperr.CodeTimeout) {
		t.Fatalf("expected CodeTimeout, got %v", err)
	}
}

// TestTasks_CtxCancelSendsCancel ctx 取消时必须尽力发送一次 tasks/cancel，并把 ctx
// 取消错误包装为 apperr.CodeCancelled 返回，而不是裸 ctx.Err()。
func TestTasks_CtxCancelSendsCancel(t *testing.T) {
	created := make(chan struct{})
	var cancelSeen atomic.Bool

	srv := newMRTRServer(func(method string, _ int, id int64, params map[string]any) string {
		switch method {
		case "tools/call":
			close(created)
			// 大间隔：确保取消发生在第一次轮询等待期间，而不是被 tasks/get 抢跑。
			return rpcResultEnvelope(id, taskCreated(5000))
		case "tasks/get":
			t.Error("tasks/get must not be reached once ctx is cancelled before the first poll wait elapses")
			return rpcResultEnvelope(id, `{"resultType":"complete","taskId":"t-1","status":"working",`+
				`"createdAt":"2020-01-01T00:00:00Z","lastUpdatedAt":"2020-01-01T00:00:00Z","ttlMs":null}`)
		case "tasks/cancel":
			cancelSeen.Store(true)
			if params["taskId"] != "t-1" {
				t.Errorf("tasks/cancel missing taskId, got %v", params)
			}
			return rpcResultEnvelope(id, `{"resultType":"complete"}`)
		default:
			return rpcResultEnvelope(id, `{"resultType":"complete"}`)
		}
	})
	c := tasksClient(srv)
	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		_, err := c.request(ctx, methodToolsCall, map[string]any{"name": "do_thing"})
		errCh <- err
	}()
	<-created
	// created 在服务端收到调用时就关闭，此刻客户端可能还没读到任务创建响应——立即取消会让
	// 调用本身失败（尚无任务，自然不发取消）。等客户端进入首次轮询等待（间隔 5s）后再取消。
	time.Sleep(200 * time.Millisecond)
	cancel()

	err := <-errCh
	if !apperr.IsCode(err, apperr.CodeCancelled) {
		t.Fatalf("expected CodeCancelled, got %v", err)
	}
	if !cancelSeen.Load() {
		t.Fatal("expected the server to receive a tasks/cancel call")
	}
}

// TestTasksCapability_ModernRequestDeclaresExtension 新纪元请求的
// _meta.clientCapabilities.extensions 必须包含 tasks（SEP-2663）。
func TestTasksCapability_ModernRequestDeclaresExtension(t *testing.T) {
	srv := newScripted(func(method string, _ map[string]any, _ http.Header) (int, http.Header, string) {
		switch method {
		case "server/discover":
			return 200, nil, `{"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{}}}`
		default:
			return 200, nil, `{"result":{"resultType":"complete","tools":[]}}`
		}
	})
	c := httpClientFor(srv)
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	meta, _ := srv.params["tools/list"]["_meta"].(map[string]any)
	caps, _ := meta[metaClientCapabilities].(map[string]any)
	exts, _ := caps["extensions"].(map[string]any)
	if _, ok := exts[extensionTasks]; !ok {
		t.Fatalf("expected extensions.%s to be declared, got %v", extensionTasks, caps)
	}
}

// TestTasksCapability_LegacyInitializeExcludesExtension 旧纪元 initialize 的
// capabilities 不得声明 tasks 扩展（旧纪元 core tasks 语义不同，本任务不实现），
// 但必须声明 MCP Apps ui 扩展（M8f-1：apps_spec.mdx 的能力声明示例即在 initialize
// 请求中给出，未区分纪元，与 tasks 扩展的纪元限定范围不同）。
func TestTasksCapability_LegacyInitializeExcludesExtension(t *testing.T) {
	srv := newScripted(func(method string, _ map[string]any, _ http.Header) (int, http.Header, string) {
		switch method {
		case "server/discover":
			return 400, nil, "Bad Request: no valid session ID provided"
		case "initialize":
			return 200, http.Header{"Mcp-Session-Id": {"s-1"}}, `{"result":{"protocolVersion":"2025-11-25","capabilities":{}}}`
		default:
			return 200, nil, `{"result":{"tools":[]}}`
		}
	})
	c := httpClientFor(srv)
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.protocolEra() != eraLegacy {
		t.Fatalf("expected legacy era, got %v", c.protocolEra())
	}
	caps, _ := srv.params["initialize"]["capabilities"].(map[string]any)
	exts, _ := caps["extensions"].(map[string]any)
	if _, ok := exts["io.modelcontextprotocol/tasks"]; ok {
		t.Fatalf("legacy initialize must not declare tasks extension, got %v", caps)
	}
	if _, ok := exts["io.modelcontextprotocol/ui"]; !ok {
		t.Fatalf("legacy initialize must declare ui extension, got %v", caps)
	}
}
