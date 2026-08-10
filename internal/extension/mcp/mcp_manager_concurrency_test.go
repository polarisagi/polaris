package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// ─── 阶段03 R-03：MCPManager.Add 长时 IO 移出写锁 回归测试 ──────────────────
//
// 复用 mcp_client_extra_test.go 中已有的 mockRoundTripperFunc，模拟一个
// initialize 方法响应缓慢（模拟真实网络/进程启动耗时）、其余方法立即返回的
// MCP HTTP 服务端。用于证明重构后 Add() 的 Connect/Initialize/ListTools
// 三步长时 IO 不再持有 m.mu 写锁——旧实现下，这段 IO 期间 CallTool/
// CallToolAsync/ListServers 等 RLock 读路径会被写锁排队阻塞，单个 MCP
// 插件重启即冻结整个工具层。

// newDelayedInitTransport 返回一个 http.RoundTripper：JSON-RPC method 为
// "initialize" 时先阻塞 delay，其余方法（tools/list、notifications/*）
// 立即返回空结果。
func newDelayedInitTransport(delay time.Duration) http.RoundTripper {
	return mockRoundTripperFunc(func(req *http.Request) *http.Response {
		var method string
		var id *int64
		if req.Body != nil {
			b, _ := io.ReadAll(req.Body)
			req.Body.Close()
			var parsed mcpRPCRequest
			if json.Unmarshal(b, &parsed) == nil {
				method = parsed.Method
				id = parsed.ID
			}
		}
		if method == "initialize" {
			time.Sleep(delay)
		}

		resp := mcpRPCResponse{JSONRPC: "2.0", ID: id}
		switch method {
		case "initialize":
			resp.Result = json.RawMessage(`{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"mock","version":"0"}}`)
		case "tools/list":
			resp.Result = json.RawMessage(`{"tools":[]}`)
		default:
			resp.Result = json.RawMessage(`{}`)
		}
		b, _ := json.Marshal(resp)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(b)),
			Header:     make(http.Header),
		}
	})
}

// TestMCPManager_Add_DoesNotBlockConcurrentReads 验证 Add() 在慢速 IO
// （initialize 耗时 250ms）期间，ListServers()（m.mu.RLock 路径）不会被
// 阻塞至与 IO 同量级耗时。重构前：整段 IO 持有 m.mu 写锁，读路径会排队
// 等待至 IO 结束；重构后：读路径应在数毫秒内返回。
func TestMCPManager_Add_DoesNotBlockConcurrentReads(t *testing.T) {
	const ioDelay = 250 * time.Millisecond
	httpClient := testSafeHTTP(newDelayedInitTransport(ioDelay))
	mgr := NewMCPManager(nil, httpClient, &mockPolicyGate{})

	done := make(chan error, 1)
	go func() {
		done <- mgr.Add(context.Background(), "slow-server", "slow-server", MCPClientConfig{
			Transport: MCPStreamableHTTP,
			URL:       "http://mock-slow",
			Timeout:   2 * time.Second,
		})
	}()

	// 留出时间让 Add() 越过"段2a"抢占 starting 标记、进入锁外 IO 阶段。
	time.Sleep(30 * time.Millisecond)

	const readerBudget = 100 * time.Millisecond // 远小于 ioDelay，证明读路径未被写锁阻塞
	for i := 0; i < 20; i++ {
		start := time.Now()
		_ = mgr.ListServers()
		if elapsed := time.Since(start); elapsed > readerBudget {
			t.Fatalf("ListServers() 第 %d 次调用耗时 %v，超过预算 %v；m.mu 写锁疑似仍覆盖长时 IO", i, elapsed, readerBudget)
		}
	}

	if err := <-done; err != nil {
		t.Fatalf("Add() 不应失败: %v", err)
	}
	servers := mgr.ListServers()
	if len(servers) != 1 || !servers[0].Connected {
		t.Fatalf("expected 1 connected server, got %+v", servers)
	}
}

// TestMCPManager_Add_ConcurrentDoubleAddSameServerID 验证同一 serverID 并发
// 两次 Add() 调用：后到者应立即被 starting 占位集合拒绝
// （CodeResourceExhausted），不产生双实例；先到者正常完成连接。
func TestMCPManager_Add_ConcurrentDoubleAddSameServerID(t *testing.T) {
	const ioDelay = 200 * time.Millisecond
	httpClient := testSafeHTTP(newDelayedInitTransport(ioDelay))
	mgr := NewMCPManager(nil, httpClient, &mockPolicyGate{})

	cfg := MCPClientConfig{
		Transport: MCPStreamableHTTP,
		URL:       "http://mock-dup",
		Timeout:   2 * time.Second,
	}

	results := make(chan error, 2)
	go func() {
		results <- mgr.Add(context.Background(), "dup-server", "dup-server", cfg)
	}()
	// 确保第二次调用在第一次已抢占 starting 标记之后发起，避免两次调用同时
	// 竞争 m.mu.Lock() 导致谁先谁后不确定——测试关心的是"后到者必被拒绝"，
	// 而非抢占顺序本身。
	time.Sleep(10 * time.Millisecond)
	go func() {
		results <- mgr.Add(context.Background(), "dup-server", "dup-server", cfg)
	}()

	err1 := <-results
	err2 := <-results

	var nilCount, exhaustedCount int
	for _, err := range []error{err1, err2} {
		switch {
		case err == nil:
			nilCount++
		case apperr.IsCode(err, apperr.CodeResourceExhausted):
			exhaustedCount++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if nilCount != 1 || exhaustedCount != 1 {
		t.Fatalf("expected exactly 1 success + 1 CodeResourceExhausted, got nilCount=%d exhaustedCount=%d (err1=%v, err2=%v)", nilCount, exhaustedCount, err1, err2)
	}

	servers := mgr.ListServers()
	if len(servers) != 1 {
		t.Fatalf("expected exactly 1 entry for dup-server (no dual instance), got %d: %+v", len(servers), servers)
	}
}
