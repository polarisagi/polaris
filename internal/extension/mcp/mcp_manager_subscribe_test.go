package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/sandbox"
)

// toolNames 读取指定 server 当前已注册的工具名（供轮询断言 refreshTools 生效）。
func toolNames(mgr *MCPManager, serverID string) []string {
	mgr.mu.RLock()
	defer mgr.mu.RUnlock()
	e, ok := mgr.entries[serverID]
	if !ok {
		return nil
	}
	names := make([]string, len(e.tools))
	for i, t := range e.tools {
		names[i] = t.Name
	}
	return names
}

// hasName 是否包含指定名称。
func hasName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// waitUntil 轮询 cond 直至为 true 或超时，超时则 t.Fatalf。
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, msg string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf(msg, args...)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newTestManager(srv http.RoundTripper) *MCPManager {
	sbx := sandbox.NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	return NewMCPManager(sbx, testSafeHTTP(srv), &mockPolicyGate{})
}

func toolJSON(names ...string) string {
	var b strings.Builder
	for i, n := range names {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"name":%q,"description":"","inputSchema":{"type":"object"}}`, n)
	}
	return b.String()
}

// ─── 旧纪元：服务器自发 notifications/tools/list_changed ─────────────────────────

// TestLegacyToolsListChanged_RefreshesRegistry 验证旧纪元下，服务器自发的
// notifications/tools/list_changed 通知（这里直接调用 dispatch 注入，模拟旧纪元
// stdio/HTTP 共用的 dispatch 管道）会异步触发 refreshTools：新增的工具被注册，
// 消失的工具被注销。
func TestLegacyToolsListChanged_RefreshesRegistry(t *testing.T) {
	var n int32
	srv := newScripted(func(method string, _ map[string]any, _ http.Header) (int, http.Header, string) {
		switch method {
		case "server/discover":
			return 400, nil, "Bad Request: legacy server" // 强制回退旧纪元
		case "initialize":
			return 200, nil, `{"result":{"protocolVersion":"2025-11-25","capabilities":{}}}`
		case "tools/list":
			if atomic.AddInt32(&n, 1) == 1 {
				return 200, nil, `{"result":{"tools":[` + toolJSON("a", "c") + `]}}`
			}
			return 200, nil, `{"result":{"tools":[` + toolJSON("a", "b") + `]}}`
		default:
			return 200, nil, `{"result":{}}`
		}
	})

	mgr := newTestManager(srv)
	if err := mgr.Add(context.Background(), "s1", "s1", MCPClientConfig{
		Transport: MCPStreamableHTTP, URL: "https://mcp.example/mcp", Timeout: 2 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	if got := toolNames(mgr, "s1"); len(got) != 2 || !hasName(got, "a") || !hasName(got, "c") {
		t.Fatalf("initial tools unexpected: %v", got)
	}

	client, ok := mgr.GetClient("s1").(*MCPClient)
	if !ok {
		t.Fatal("GetClient did not return *MCPClient")
	}
	if client.protocolEra() != eraLegacy {
		t.Fatalf("test setup expects legacy era, got %v", client.protocolEra())
	}
	client.dispatch(&mcpRPCResponse{Method: notificationToolsListChanged, Params: json.RawMessage(`{}`)})

	waitUntil(t, 2*time.Second, func() bool {
		got := toolNames(mgr, "s1")
		return len(got) == 2 && hasName(got, "a") && hasName(got, "b") && !hasName(got, "c")
	}, "timeout waiting for legacy list_changed refresh, tools=%v", toolNames(mgr, "s1"))
}

// ─── 合并：短时间内连续多个通知只触发有限次数的 ListTools ─────────────────────────

// TestRefreshTools_MergesConcurrentNotifications 直接压测 refreshTools 的合并逻辑：
// 5 个并发调用应当把慢速 ListTools 合并为至多 2 次实际请求（1 次进行中 + 至多 1 次
// 合并后补跑），而不是 5 次都各自发起网络调用。
func TestRefreshTools_MergesConcurrentNotifications(t *testing.T) {
	var calls int32
	gate := make(chan struct{})
	srv := newScripted(func(method string, _ map[string]any, _ http.Header) (int, http.Header, string) {
		if method != "tools/list" {
			return 200, nil, `{"result":{}}`
		}
		atomic.AddInt32(&calls, 1)
		<-gate // 阻塞直到测试放行，制造"并发窗口内多次通知到达"的场景
		return 200, nil, `{"result":{"tools":[]}}`
	})

	mgr := newTestManager(nil)
	testClient := NewMCPClient(MCPClientConfig{
		Transport: MCPStreamableHTTP, URL: "http://mock", Timeout: 5 * time.Second,
	}, testSafeHTTP(srv))
	mgr.mu.Lock()
	mgr.entries["fake-1"] = &mcpEntry{name: "fake-1", client: testClient, tools: nil}
	mgr.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := mgr.refreshTools(context.Background(), "fake-1"); err != nil {
				t.Errorf("refreshTools returned error: %v", err)
			}
		}()
	}
	// 留出时间让 5 次调用都完成"进入合并态"的快速判定（第一次会阻塞在 gate 上，
	// 其余 4 次应立即把 pending 置位后返回，不发起网络调用）。
	time.Sleep(80 * time.Millisecond)
	close(gate)
	wg.Wait()
	// 合并后若确有 pending，会再补跑一轮；给它留够时间跑完。
	time.Sleep(100 * time.Millisecond)

	if got := atomic.LoadInt32(&calls); got < 1 || got > 2 {
		t.Fatalf("expected 1~2 ListTools calls under concurrent refresh, got %d", got)
	}
}

// ─── 新纪元：未声明 listChanged 时不发送 subscriptions/listen ───────────────────

func TestSubscription_NotStartedWhenUndeclared(t *testing.T) {
	var subscribeCalls int32
	srv := newScripted(func(method string, _ map[string]any, _ http.Header) (int, http.Header, string) {
		switch method {
		case "server/discover":
			return 200, nil, `{"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{}}}`
		case "tools/list":
			return 200, nil, `{"result":{"tools":[]}}`
		case methodSubscriptionsListen:
			atomic.AddInt32(&subscribeCalls, 1)
			return 200, nil, `{"result":{}}`
		default:
			return 200, nil, `{"result":{}}`
		}
	})

	mgr := newTestManager(srv)
	if err := mgr.Add(context.Background(), "s1", "s1", MCPClientConfig{
		Transport: MCPStreamableHTTP, URL: "https://mcp.example/mcp", Timeout: 2 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond) // 给可能误触发的订阅协程留出运行窗口
	if got := atomic.LoadInt32(&subscribeCalls); got != 0 {
		t.Fatalf("server did not declare toolsListChanged; subscriptions/listen must not be sent, got %d calls", got)
	}
}

// ─── 新纪元 Streamable HTTP：ack + list_changed 触发刷新，graceful closure 不重连 ──

// subscribeFakeServer 复用 scriptedServer 的非流式调度风格（handle 回调返回 JSON-RPC
// 结果），并为 subscriptions/listen 额外支持流式 SSE 响应：onListen 在独立 goroutine 里
// 通过 io.PipeWriter 写入事件、决定这次尝试何时以何种方式结束。
type subscribeFakeServer struct {
	mu       sync.Mutex
	handle   func(method string, params map[string]any) (int, string)
	onListen func(attempt int, reqID int64, w *io.PipeWriter)
	attempts int32
	headers  map[string]http.Header
	params   map[string]map[string]any
}

func (s *subscribeFakeServer) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	var rpc struct {
		ID     *int64         `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	_ = json.Unmarshal(body, &rpc)
	s.mu.Lock()
	s.headers[rpc.Method] = req.Header.Clone()
	s.params[rpc.Method] = rpc.Params
	s.mu.Unlock()

	if rpc.Method == methodSubscriptionsListen {
		attempt := int(atomic.AddInt32(&s.attempts, 1))
		pr, pw := io.Pipe()
		if s.onListen != nil {
			go s.onListen(attempt, *rpc.ID, pw)
		} else {
			pw.Close()
		}
		return &http.Response{StatusCode: http.StatusOK,
			Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: pr, Request: req}, nil
	}

	status, payload := s.handle(rpc.Method, rpc.Params)
	if rpc.ID != nil && strings.HasPrefix(payload, "{") && !strings.Contains(payload, `"jsonrpc"`) {
		payload = `{"jsonrpc":"2.0","id":` + jsonInt(*rpc.ID) + `,` + payload[1:]
	}
	hdr := http.Header{"Content-Type": {"application/json"}}
	return &http.Response{StatusCode: status, Header: hdr, Body: io.NopCloser(strings.NewReader(payload)), Request: req}, nil
}

func ackEvent(reqID int64) string {
	return fmt.Sprintf("data: {\"jsonrpc\":\"2.0\",\"method\":%q,\"params\":{\"_meta\":{%q:%d},\"notifications\":{\"toolsListChanged\":true}}}\n\n",
		notificationSubscriptionsAcknowledged, metaSubscriptionID, reqID)
}

func listChangedEvent(reqID int64) string {
	return fmt.Sprintf("data: {\"jsonrpc\":\"2.0\",\"method\":%q,\"params\":{\"_meta\":{%q:%d}}}\n\n",
		notificationToolsListChanged, metaSubscriptionID, reqID)
}

func gracefulCloseEvent(reqID int64) string {
	return fmt.Sprintf("data: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"resultType\":\"complete\"}}\n\n", reqID)
}

func TestSubscription_HTTP_AckAndListChangedTriggersRefresh(t *testing.T) {
	var toolsListN int32
	srv := &subscribeFakeServer{headers: map[string]http.Header{}, params: map[string]map[string]any{}}
	srv.handle = func(method string, _ map[string]any) (int, string) {
		switch method {
		case "server/discover":
			return 200, `{"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{"listChanged":true}}}}`
		case "tools/list":
			if atomic.AddInt32(&toolsListN, 1) == 1 {
				return 200, `{"result":{"tools":[` + toolJSON("a") + `]}}`
			}
			return 200, `{"result":{"tools":[` + toolJSON("a", "b") + `]}}`
		default:
			return 200, `{"result":{}}`
		}
	}

	closeOnce := make(chan struct{})
	srv.onListen = func(_ int, reqID int64, w *io.PipeWriter) {
		defer w.Close()
		io.WriteString(w, ackEvent(reqID))
		io.WriteString(w, listChangedEvent(reqID))
		<-closeOnce // 断言完刷新结果后再优雅关闭，避免与后续断言竞争
		io.WriteString(w, gracefulCloseEvent(reqID))
	}

	mgr := newTestManager(srv)
	if err := mgr.Add(context.Background(), "s1", "s1", MCPClientConfig{
		Transport: MCPStreamableHTTP, URL: "https://mcp.example/mcp", Timeout: 2 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}

	waitUntil(t, 2*time.Second, func() bool {
		got := toolNames(mgr, "s1")
		return len(got) == 2 && hasName(got, "a") && hasName(got, "b")
	}, "timeout waiting for subscription-triggered refresh, tools=%v", toolNames(mgr, "s1"))

	close(closeOnce)
	waitUntil(t, 2*time.Second, func() bool {
		return atomic.LoadInt32(&srv.attempts) == 1
	}, "graceful closure handling did not complete in time")
	time.Sleep(50 * time.Millisecond) // 优雅关闭后不应立即又发起新的 subscriptions/listen

	srv.mu.Lock()
	h := srv.headers[methodSubscriptionsListen]
	p := srv.params[methodSubscriptionsListen]
	srv.mu.Unlock()
	if h.Get("Mcp-Method") != methodSubscriptionsListen {
		t.Fatalf("Mcp-Method header missing or wrong: %v", h)
	}
	notif, _ := p["notifications"].(map[string]any)
	if notif["toolsListChanged"] != true {
		t.Fatalf("subscriptions/listen params missing notifications.toolsListChanged: %v", p)
	}
	if got := atomic.LoadInt32(&srv.attempts); got != 1 {
		t.Fatalf("graceful closure must not reconnect, attempts=%d", got)
	}
}

// TestSubscription_HTTP_AbnormalDisconnectReconnects 验证异常断开（无优雅关闭响应，
// 流直接结束）会触发指数退避重连；测试把退避区间调短以避免拖慢测试。
func TestSubscription_HTTP_AbnormalDisconnectReconnects(t *testing.T) {
	srv := &subscribeFakeServer{headers: map[string]http.Header{}, params: map[string]map[string]any{}}
	srv.handle = func(method string, _ map[string]any) (int, string) {
		switch method {
		case "server/discover":
			return 200, `{"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{"listChanged":true}}}}`
		case "tools/list":
			return 200, `{"result":{"tools":[]}}`
		default:
			return 200, `{"result":{}}`
		}
	}

	readyToDisconnect := make(chan struct{})
	srv.onListen = func(attempt int, reqID int64, w *io.PipeWriter) {
		defer w.Close()
		io.WriteString(w, ackEvent(reqID))
		if attempt == 1 {
			<-readyToDisconnect // 等测试把 backoff 调短后再断开，避免默认 60s 上限拖慢测试
			return              // 不发优雅关闭响应，直接关闭 pipe → 异常断开
		}
		io.WriteString(w, gracefulCloseEvent(reqID))
	}

	mgr := newTestManager(srv)
	if err := mgr.Add(context.Background(), "s1", "s1", MCPClientConfig{
		Transport: MCPStreamableHTTP, URL: "https://mcp.example/mcp", Timeout: 2 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}

	client, ok := mgr.GetClient("s1").(*MCPClient)
	if !ok {
		t.Fatal("GetClient did not return *MCPClient")
	}
	waitUntil(t, time.Second, func() bool { return atomic.LoadInt32(&srv.attempts) >= 1 },
		"first subscription attempt did not arrive")
	client.subscriptionBackoffInitial = 5 * time.Millisecond
	client.subscriptionBackoffMax = 20 * time.Millisecond
	close(readyToDisconnect)

	waitUntil(t, 2*time.Second, func() bool { return atomic.LoadInt32(&srv.attempts) >= 2 },
		"abnormal disconnect did not trigger a reconnect, attempts=%d", atomic.LoadInt32(&srv.attempts))
}
