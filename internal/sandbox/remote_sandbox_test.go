package sandbox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestRemoteSandbox_RunSuccess(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("expected Bearer test-token, got %s", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected application/json, got %s", r.Header.Get("Content-Type"))
		}
		if r.URL.Path != "/execute" {
			t.Errorf("expected /execute, got %s", r.URL.Path)
		}

		res := types.ToolResult{
			Success: true,
			Output:  []byte("remote output"),
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(res)
	}))
	defer ts.Close()

	sandbox := NewRemoteSandbox(ts.URL, "test-token", 5, ts.Client())
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
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer ts.Close()

	sandbox := NewRemoteSandbox(ts.URL, "", 1, ts.Client())
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
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("invalid json"))
	}))
	defer ts.Close()

	sandbox := NewRemoteSandbox(ts.URL, "", 1, ts.Client())
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
