package adapter

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// markedTransport 记录经过它的请求次数，用于断言出站请求确实走了"受保护"的
// Transport，而非裸 http.Client / http.DefaultTransport（S-03 回归锚点）。
type markedTransport struct {
	calls int
}

func (m *markedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	m.calls++
	// "{}" 是一个对 matrixSync/tgGetUpdates 的 JSON 解码均合法（零值）的最小
	// 响应体；本测试只关心请求是否经过了受保护 Transport，不关心业务解码结果。
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("{}")),
		Header:     make(http.Header),
	}, nil
}

// TestDeriveClient_NilBase_FailClosed 验证 deriveClient(nil, ...) fail-closed 返回 error，
// 不得退化为 http.DefaultClient。
func TestDeriveClient_NilBase_FailClosed(t *testing.T) {
	c, err := deriveClient(nil, time.Second)
	if err == nil {
		t.Fatal("expected error for nil base client, got nil")
	}
	if c != nil {
		t.Fatalf("expected nil client on error, got %+v", c)
	}
}

// TestDeriveClient_PreservesTransportOverridesTimeout 验证 deriveClient 只覆盖
// Timeout，Transport 原样继承。
func TestDeriveClient_PreservesTransportOverridesTimeout(t *testing.T) {
	base := &http.Client{Transport: &markedTransport{}, Timeout: 99 * time.Second}
	derived, err := deriveClient(base, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if derived.Transport != base.Transport {
		t.Errorf("Transport must be inherited unchanged, got %+v want %+v", derived.Transport, base.Transport)
	}
	if derived.Timeout != 5*time.Second {
		t.Errorf("Timeout should be overridden to 5s, got %v", derived.Timeout)
	}
}

// TestMatrixSync_UsesInjectedTransport 验证 matrixSync 使用调用方传入的受保护
// client（而非自建裸 http.Client）。回归锚点：修复前 matrixSync 会新建
// syncClient，本测试的 markedTransport 记不到任何调用。
func TestMatrixSync_UsesInjectedTransport(t *testing.T) {
	mt := &markedTransport{}
	client := &http.Client{Transport: mt}
	_, _, err := matrixSync(context.Background(), client, "http://homeserver.example", "token", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mt.calls != 1 {
		t.Fatalf("expected matrixSync to route through injected Transport exactly once, got %d calls", mt.calls)
	}
}

// TestSignalReceiveSSE_UsesHostTransport 验证 signalReceiveSSE 通过
// host.HTTPClient() 的 Transport 发起请求（而非新建裸 http.Client）。
func TestSignalReceiveSSE_UsesHostTransport(t *testing.T) {
	mt := &markedTransport{}
	host := &mockPollerHost{mockClient: &http.Client{Transport: mt}}
	err := signalReceiveSSE(context.Background(), host, "ch", "http://signal.example", "acct", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mt.calls != 1 {
		t.Fatalf("expected signalReceiveSSE to route through host Transport exactly once, got %d calls", mt.calls)
	}
}

// TestNewTelegramPoller_UsesBaseTransport 验证 NewTelegramPoller 派生的 client
// 复用传入的受保护 Transport。
func TestNewTelegramPoller_UsesBaseTransport(t *testing.T) {
	mt := &markedTransport{}
	base := &http.Client{Transport: mt}
	poller, err := NewTelegramPoller(base)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := tgGetUpdates(context.Background(), poller.httpClient, "token", 0); err == nil {
		// mock 返回空 body，json.Unmarshal 会失败并返回 error——这里只关心
		// Transport 是否被调用，忽略业务层面的 decode 结果。
		t.Logf("tgGetUpdates unexpectedly succeeded against empty mock body")
	}
	if mt.calls != 1 {
		t.Fatalf("expected tgGetUpdates to route through base Transport exactly once, got %d calls", mt.calls)
	}
}

// TestNewTelegramPoller_NilBase_FailClosed 验证 base==nil 时 fail-closed。
func TestNewTelegramPoller_NilBase_FailClosed(t *testing.T) {
	poller, err := NewTelegramPoller(nil)
	if err == nil {
		t.Fatal("expected error for nil base client, got nil")
	}
	if poller != nil {
		t.Fatalf("expected nil poller on error, got %+v", poller)
	}
}
