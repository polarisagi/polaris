package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// scriptedServer 按方法名应答的 Streamable HTTP 假服务器，记录每个请求的头与参数。
type scriptedServer struct {
	mu      sync.Mutex
	handle  func(method string, params map[string]any, h http.Header) (int, http.Header, string)
	headers map[string]http.Header
	params  map[string]map[string]any
}

func (s *scriptedServer) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	var rpc struct {
		ID     *int64         `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	_ = json.Unmarshal(body, &rpc)
	s.mu.Lock()
	s.headers[rpc.Method], s.params[rpc.Method] = req.Header.Clone(), rpc.Params
	s.mu.Unlock()
	status, hdr, payload := s.handle(rpc.Method, rpc.Params, req.Header)
	if rpc.ID != nil && strings.HasPrefix(payload, "{") && !strings.Contains(payload, `"jsonrpc"`) {
		payload = `{"jsonrpc":"2.0","id":` + jsonInt(*rpc.ID) + `,` + payload[1:]
	}
	if hdr == nil {
		hdr = http.Header{}
	}
	hdr.Set("Content-Type", "application/json")
	return &http.Response{StatusCode: status, Header: hdr, Body: io.NopCloser(bytes.NewBufferString(payload)), Request: req}, nil
}

func jsonInt(i int64) string { b, _ := json.Marshal(i); return string(b) }

func newScripted(h func(string, map[string]any, http.Header) (int, http.Header, string)) *scriptedServer {
	return &scriptedServer{handle: h, headers: map[string]http.Header{}, params: map[string]map[string]any{}}
}

func httpClientFor(s *scriptedServer) *MCPClient {
	return NewMCPClient(MCPClientConfig{ServerName: "t", Transport: MCPStreamableHTTP, URL: "https://mcp.example/mcp"}, testSafeHTTP(s))
}

func TestModernEra_DiscoverMetaAndHeaders(t *testing.T) {
	srv := newScripted(func(method string, _ map[string]any, _ http.Header) (int, http.Header, string) {
		switch method {
		case "server/discover":
			return 200, nil, `{"result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{"tools":{}},"instructions":"use me"}}`
		case "tools/list":
			return 200, nil, `{"result":{"resultType":"complete","tools":[
			  {"name":"sql","inputSchema":{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"},"q":{"type":"string"}}}},
			  {"name":"bad","inputSchema":{"type":"object","properties":{"xs":{"type":"array","items":{"type":"string","x-mcp-header":"X"}}}}}]}}`
		default:
			return 200, nil, `{"result":{"resultType":"complete","content":[{"type":"text","text":"ok"}]}}`
		}
	})
	c := httpClientFor(srv)
	ctx := context.Background()
	if err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	if c.protocolEra() != eraModern || c.ServerMeta().Instructions != "use me" {
		t.Fatalf("era %v meta %+v", c.protocolEra(), c.ServerMeta())
	}
	tools, err := c.ListTools(ctx)
	if err != nil || len(tools) != 1 || tools[0].Name != "sql" {
		t.Fatalf("invalid x-mcp-header tool must be excluded: %+v %v", tools, err)
	}
	if _, _, err := c.CallTool(ctx, "sql", map[string]any{"region": "us-west1", "q": "select 1"}); err != nil {
		t.Fatal(err)
	}
	h := srv.headers["tools/call"]
	if h.Get("MCP-Protocol-Version") != "2026-07-28" || h.Get("Mcp-Method") != "tools/call" || h.Get("Mcp-Name") != "sql" ||
		h.Get("Mcp-Param-Region") != "us-west1" || h.Get("Mcp-Session-Id") != "" {
		t.Fatalf("modern headers: %v", h)
	}
	meta, _ := srv.params["tools/call"]["_meta"].(map[string]any)
	if meta[metaProtocolVersion] != "2026-07-28" || meta[metaClientCapabilities] == nil || meta[metaClientInfo] == nil {
		t.Fatalf("per-request _meta: %v", meta)
	}
}

func TestLegacyFallback_PlainErrorAndSession(t *testing.T) {
	srv := newScripted(func(method string, _ map[string]any, _ http.Header) (int, http.Header, string) {
		switch method {
		case "server/discover":
			return 400, nil, "Bad Request: no valid session ID provided"
		case "initialize":
			return 200, http.Header{"Mcp-Session-Id": {"s-123"}}, `{"result":{"protocolVersion":"2025-11-25","capabilities":{}}}`
		default:
			return 200, nil, `{"result":{"tools":[]}}`
		}
	})
	c := httpClientFor(srv)
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListTools(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := srv.headers["tools/list"]
	if c.protocolEra() != eraLegacy || h.Get("Mcp-Session-Id") != "s-123" || h.Get("MCP-Protocol-Version") != "2025-11-25" || h.Get("Mcp-Method") != "" {
		t.Fatalf("legacy headers: era=%v %v", c.protocolEra(), h)
	}
	if _, hasMeta := srv.params["tools/list"]["_meta"]; hasMeta {
		t.Fatal("legacy requests carry no modern _meta")
	}
}

func TestModernError_UnsupportedVersionSelectsLegacy(t *testing.T) {
	srv := newScripted(func(method string, _ map[string]any, _ http.Header) (int, http.Header, string) {
		if method == "server/discover" {
			return 400, nil, `{"error":{"code":-32022,"message":"Unsupported protocol version","data":{"supported":["2025-11-25"],"requested":"2026-07-28"}}}`
		}
		return 200, nil, `{"result":{"protocolVersion":"2025-11-25","capabilities":{}}}`
	})
	c := httpClientFor(srv)
	if err := c.Initialize(context.Background()); err != nil || c.protocolEra() != eraLegacy {
		t.Fatalf("dual-era server must negotiate the legacy version: %v %v", err, c.protocolEra())
	}
	srv2 := newScripted(func(string, map[string]any, http.Header) (int, http.Header, string) {
		return 400, nil, `{"error":{"code":-32022,"message":"Unsupported protocol version","data":{"supported":["2099-01-01"]}}}`
	})
	if err := httpClientFor(srv2).Initialize(context.Background()); err == nil {
		t.Fatal("no mutually supported version must fail instead of falling back")
	}
}

func TestHeaderEncodingAndAnnotations(t *testing.T) {
	for in, want := range map[string]string{"us-west1": "us-west1", "Hello, 世界": "=?base64?SGVsbG8sIOS4lueVjA==?=",
		" padded ": "=?base64?IHBhZGRlZCA=?=", "line1\nline2": "=?base64?bGluZTEKbGluZTI=?=",
		"=?base64?literal?=": "=?base64?PT9iYXNlNjQ/bGl0ZXJhbD89?="} {
		if got := encodeHeaderValue(in); got != want {
			t.Errorf("%q: %q want %q", in, got, want)
		}
	}
	var schema any
	_ = json.Unmarshal([]byte(`{"type":"object","properties":{"a":{"type":"object","properties":{"b":{"type":"integer","x-mcp-header":"B"}}},
	  "c":{"type":"number","x-mcp-header":"C"}}}`), &schema)
	if _, ok := toolHeaderParams(schema); ok {
		t.Fatal("number-typed header parameter must invalidate the tool")
	}
	_ = json.Unmarshal([]byte(`{"type":"object","properties":{"a":{"type":"object","properties":{"b":{"type":"integer","x-mcp-header":"B"}}}}}`), &schema)
	hp, ok := toolHeaderParams(schema)
	if !ok || len(hp) != 1 || strings.Join(hp[0].Path, ".") != "a.b" {
		t.Fatalf("nested properties chain is allowed: %+v %v", hp, ok)
	}
	if v, ok := headerValueOf(map[string]any{"a": map[string]any{"b": float64(42)}}, hp[0].Path); !ok || v != "42" {
		t.Fatalf("integer value: %q %v", v, ok)
	}
}
