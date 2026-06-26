package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMCPClient_CallToolTainted(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		b, _ := io.ReadAll(r.Body)
		var req mcpRPCRequest
		json.Unmarshal(b, &req)

		resp := mcpRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
		}
		if req.Method == "tools/call" {
			resp.Result = json.RawMessage(`{"content":[{"type":"text","text":"tainted output"}],"isError":false}`)
		}

		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	cfg := MCPClientConfig{
		Transport: MCPStreamableHTTP,
		URL:       ts.URL,
		Timeout:   2 * time.Second,
		Trusted:   false,
	}
	client := NewMCPClient(cfg, ts.Client())

	ctx := context.Background()
	text, imgs, maxTaint, err := client.CallToolTainted(ctx, "my_tool", map[string]any{"arg": "val"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "tainted output" {
		t.Errorf("expected tainted output, got %q", text)
	}
	if len(imgs) != 0 {
		t.Errorf("expected 0 images")
	}
	// Untrusted → TaintHigh expected
	if maxTaint < 2 { // types.TaintHigh normally is 2
		t.Errorf("expected maxTaint >= 2, got %d", maxTaint)
	}
}

func TestMCPClient_SSE(t *testing.T) {
	var sseCh chan string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sse") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// Send endpoint event
			fmt.Fprintf(w, "event: endpoint\ndata: %s/post\n\n", "http://"+r.Host)
			w.(http.Flusher).Flush()

			for {
				select {
				case <-r.Context().Done():
					return
				case msg := <-sseCh:
					fmt.Fprintf(w, "%s", msg)
					w.(http.Flusher).Flush()
				}
			}
		}
		if strings.HasSuffix(r.URL.Path, "/post") {
			b, _ := io.ReadAll(r.Body)
			var req mcpRPCRequest
			json.Unmarshal(b, &req)

			// push an event to the sse stream to complete the RPC call
			respJSON := fmt.Sprintf(`{"jsonrpc":"2.0", "id":%d, "result":{"tools":[]}}`, *req.ID)
			sseCh <- fmt.Sprintf("event: message\ndata: %s\n\n", respJSON)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	sseCh = make(chan string, 1)

	cfg := MCPClientConfig{
		Transport: MCPSSE,
		URL:       ts.URL,
		Timeout:   2 * time.Second,
	}
	client := NewMCPClient(cfg, ts.Client())
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := client.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	if len(tools) != 0 {
		t.Errorf("expected 0 tools, got %d", len(tools))
	}
}

func TestMCPClient_Dispatch_ServerRequest(t *testing.T) {
	client := NewMCPClient(MCPClientConfig{Transport: MCPStreamableHTTP}, http.DefaultClient)
	var called int32
	client.SetServerRequestHandler(func(ctx context.Context, method string, id int64, params json.RawMessage) (json.RawMessage, error) {
		atomic.StoreInt32(&called, 1)
		return json.RawMessage(`{"status":"ok"}`), nil
	})

	id := int64(999)
	req := mcpRPCResponse{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  "roots/list",
		Params:  json.RawMessage(`{}`),
	}

	// Simulate receiving a server request
	client.dispatch(&req)

	// wait a bit for async goroutine to execute
	time.Sleep(100 * time.Millisecond)

	if atomic.LoadInt32(&called) == 0 {
		t.Errorf("expected server request handler to be called")
	}
}

func TestMCPClient_ConnectStdio(t *testing.T) {
	cfg := MCPClientConfig{
		Transport: MCPStdio,
		Command:   "/bin/cat",
	}
	client := NewMCPClient(cfg, nil)
	err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("connectStdio failed: %v", err)
	}

	// Simulate an incoming valid RPC response
	respBytes := []byte(`{"jsonrpc":"2.0", "id":1, "result":{}}`)
	client.stdin.Write(append(respBytes, '\n'))

	// Also test sending a notification to cover send() stdio branch
	client.notify(context.Background(), "test/notification", nil)

	time.Sleep(100 * time.Millisecond) // Let readLoop process it
	client.Close()
}

func TestMCPClient_CallTool(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		b, _ := io.ReadAll(r.Body)
		var req mcpRPCRequest
		json.Unmarshal(b, &req)

		resp := mcpRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
		}
		if req.Method == "tools/call" {
			resp.Result = json.RawMessage(`{"content":[{"type":"text","text":"normal output"}],"isError":false}`)
		}

		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	cfg := MCPClientConfig{
		Transport: MCPStreamableHTTP,
		URL:       ts.URL,
	}
	client := NewMCPClient(cfg, ts.Client())

	text, imgs, err := client.CallTool(context.Background(), "my_tool", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if text != "normal output" {
		t.Errorf("expected normal output, got %q", text)
	}
	if len(imgs) != 0 {
		t.Errorf("expected 0 images")
	}
}
