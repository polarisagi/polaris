package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestRemoteSandbox_RunSuccess(t *testing.T) {
	client := &http.Client{
		Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
			if req.Header.Get("Authorization") != "Bearer test-token" {
				t.Errorf("expected Bearer test-token, got %s", req.Header.Get("Authorization"))
			}
			if req.Header.Get("Content-Type") != "application/json" {
				t.Errorf("expected application/json, got %s", req.Header.Get("Content-Type"))
			}
			if req.URL.Path != "/execute" {
				t.Errorf("expected /execute, got %s", req.URL.Path)
			}

			res := types.ToolResult{
				Success: true,
				Output:  []byte("remote output"),
			}
			b, _ := json.Marshal(res)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(b)),
				Header:     make(http.Header),
			}
		}),
	}

	sandbox := NewRemoteSandbox("http://dummy", "test-token", 5, client)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	spec := SandboxSpec{
		ToolName: "remote-tool",
		Input:    []byte(`{"key":"value"}`),
	}
	result, err := sandbox.Run(ctx, spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !result.Success {
		t.Errorf("expected success, got false")
	}
	if string(result.Output) != "remote output" {
		t.Errorf("expected remote output, got %s", result.Output)
	}
}

func TestRemoteSandbox_RunHTTPError(t *testing.T) {
	client := &http.Client{
		Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Body:       io.NopCloser(bytes.NewBufferString("internal error")),
				Header:     make(http.Header),
			}
		}),
	}

	sandbox := NewRemoteSandbox("http://dummy", "", 1, client)
	ctx := context.Background()

	result, err := sandbox.Run(ctx, SandboxSpec{ToolName: "test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err) // We expect a result with Success: false
	}
	if result.Success {
		t.Errorf("expected failure, got success")
	}
	if result.Error == "" {
		t.Errorf("expected error message")
	}
}

func TestRemoteSandbox_RunJSONParseError(t *testing.T) {
	client := &http.Client{
		Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewBufferString("invalid json")),
				Header:     make(http.Header),
			}
		}),
	}

	sandbox := NewRemoteSandbox("http://dummy", "", 1, client)
	ctx := context.Background()

	_, err := sandbox.Run(ctx, SandboxSpec{ToolName: "test"})
	if err == nil {
		t.Fatalf("expected error from unmarshaling, got nil")
	}
}

func TestRemoteSandbox_NewRemoteSandboxDefaults(t *testing.T) {
	sandbox := NewRemoteSandbox("http://example.com", "", 0, nil)
	if sandbox.httpClient == nil {
		t.Errorf("expected default http client")
	}
	// The timeout should be 300s
	if sandbox.httpClient.Timeout != 300*time.Second {
		t.Errorf("expected 300s timeout, got %v", sandbox.httpClient.Timeout)
	}
}

func TestRemoteSandbox_RunRequestBuildError(t *testing.T) {
	sandbox := NewRemoteSandbox("::invalid-url::", "", 1, nil)
	// NewRequestWithContext will fail with bad url or nil context
	// Actually nil context causes an error in Go 1.13+
	var ctx context.Context // nil context
	_, err := sandbox.Run(ctx, SandboxSpec{ToolName: "test"})
	if err == nil {
		t.Fatalf("expected error building request")
	}
}

func TestRemoteSandbox_RunRequestDoError(t *testing.T) {
	// A valid URL that points to a dead port
	sandbox := NewRemoteSandbox("http://127.0.0.1:0", "", 1, nil)
	ctx := context.Background()
	result, err := sandbox.Run(ctx, SandboxSpec{ToolName: "test"})
	if err != nil {
		t.Fatalf("unexpected error, should return ToolResult with error: %v", err)
	}
	if result.Success {
		t.Errorf("expected failure, got success")
	}
	if result.Error == "" {
		t.Errorf("expected error message")
	}
}

type mockRoundTripperFunc func(req *http.Request) *http.Response

func (f mockRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req), nil
}
