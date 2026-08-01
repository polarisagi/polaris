package connector

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

type notionMockTransport struct {
	calls int
	body  string
}

func (m *notionMockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	m.calls++
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(m.body)),
		Header:     make(http.Header),
	}, nil
}

// TestNewNotionConnector_NilHTTPClient_FailClosed 验证未注入受保护 client 时
// 构造失败（S-04，禁止退化为 http.DefaultClient）。
func TestNewNotionConnector_NilHTTPClient_FailClosed(t *testing.T) {
	conn, err := NewNotionConnector(func(ctx context.Context) (string, error) {
		return "tok", nil
	}, nil)
	if err == nil {
		t.Fatal("expected error when httpClient is nil, got nil")
	}
	if conn != nil {
		t.Fatalf("expected nil connector on error, got %+v", conn)
	}
	if !apperr.IsCode(err, apperr.CodeInternal) {
		t.Errorf("expected CodeInternal, got %v", apperr.CodeOf(err))
	}
}

// TestNotionConnector_RequestsRouteThroughInjectedTransport 验证注入的 mock
// Transport 确实承载了 Notion API 请求（回归锚点：修复前 notionapi.NewClient
// 未注入任何 client，请求走 http.DefaultClient，完全绕过 SafeDialer/PolicyGate）。
func TestNotionConnector_RequestsRouteThroughInjectedTransport(t *testing.T) {
	mt := &notionMockTransport{body: `{"results":[],"has_more":false,"next_cursor":null}`}
	client := &http.Client{Transport: mt}

	conn, err := NewNotionConnector(func(ctx context.Context) (string, error) {
		return "tok", nil
	}, client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := conn.List(context.Background()); err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if mt.calls != 1 {
		t.Fatalf("expected exactly 1 request through injected Transport, got %d", mt.calls)
	}
}
