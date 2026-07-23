package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type mockRoundTripperFunc func(req *http.Request) *http.Response

func (f mockRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req), nil
}

func TestMCPClient_CallToolTainted(t *testing.T) {
	clientHTTP := &http.Client{
		Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
			resp := mcpRPCResponse{
				JSONRPC: "2.0",
			}
			// Simulate receiving a JSON-RPC request and getting its ID
			if req.Body != nil {
				b, _ := io.ReadAll(req.Body)
				var mcpReq mcpRPCRequest
				if json.Unmarshal(b, &mcpReq) == nil {
					resp.ID = mcpReq.ID
					if mcpReq.Method == "tools/call" {
						resp.Result = json.RawMessage(`{"content":[{"type":"text","text":"tainted output"}],"isError":false}`)
					}
				}
			}

			b, _ := json.Marshal(resp)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(string(b))),
				Header:     make(http.Header),
			}
		}),
	}

	cfg := MCPClientConfig{
		Transport: MCPStreamableHTTP,
		URL:       "http://dummy",
		Timeout:   2 * time.Second,
		Trusted:   false,
	}
	client := NewMCPClient(cfg, clientHTTP)

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
	sseCh = make(chan string, 1)

	clientHTTP := &http.Client{
		Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
			if strings.HasSuffix(req.URL.Path, "/sse") {
				pr, pw := io.Pipe()
				go func() {
					fmt.Fprintf(pw, "event: endpoint\ndata: %s/post\n\n", "http://"+req.Host)
					for {
						select {
						case <-req.Context().Done():
							pw.Close()
							return
						case msg := <-sseCh:
							fmt.Fprintf(pw, "%s", msg)
						}
					}
				}()
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       pr,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				}
			}
			if strings.HasSuffix(req.URL.Path, "/post") {
				b, _ := io.ReadAll(req.Body)
				var mcpReq mcpRPCRequest
				json.Unmarshal(b, &mcpReq)

				respJSON := fmt.Sprintf(`{"jsonrpc":"2.0", "id":%d, "result":{"tools":[]}}`, *mcpReq.ID)
				sseCh <- fmt.Sprintf("event: message\ndata: %s\n\n", respJSON)
				return &http.Response{
					StatusCode: http.StatusAccepted,
					Body:       io.NopCloser(strings.NewReader("")),
				}
			}
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(strings.NewReader("")),
			}
		}),
	}

	cfg := MCPClientConfig{
		Transport: MCPSSE,
		URL:       "http://dummy",
		Timeout:   2 * time.Second,
	}
	client := NewMCPClient(cfg, clientHTTP)
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
		Transport:     MCPStdio,
		Command:       "/bin/cat",
		SandboxPolicy: "none",
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
	clientHTTP := &http.Client{
		Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
			resp := mcpRPCResponse{
				JSONRPC: "2.0",
			}
			if req.Body != nil {
				b, _ := io.ReadAll(req.Body)
				var mcpReq mcpRPCRequest
				if json.Unmarshal(b, &mcpReq) == nil {
					resp.ID = mcpReq.ID
					if mcpReq.Method == "tools/call" {
						resp.Result = json.RawMessage(`{"content":[{"type":"text","text":"normal output"}],"isError":false}`)
					}
				}
			}

			b, _ := json.Marshal(resp)
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(string(b))),
				Header:     make(http.Header),
			}
		}),
	}

	cfg := MCPClientConfig{
		Transport: MCPStreamableHTTP,
		URL:       "http://dummy",
	}
	client := NewMCPClient(cfg, clientHTTP)

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
