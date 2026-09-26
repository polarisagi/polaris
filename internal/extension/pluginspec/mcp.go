package pluginspec

import (
	"encoding/json"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
)

// MCP 传输类型（归一化后）。"streamable-http" / "streamable_http" 统一为 http。
const (
	MCPTypeStdio = "stdio"
	MCPTypeHTTP  = "http"
	MCPTypeSSE   = "sse"
	MCPTypeWS    = "ws"
)

const (
	RuleMCPParse     = "mcp.parse"
	RuleMCPType      = "mcp.type"
	RuleMCPCommand   = "mcp.stdio.command"
	RuleMCPURL       = "mcp.remote.url"
	RuleMCPHTTPS     = "agent-plugins.mcp.https"
	RuleMCPSSELegacy = "mcp.sse.deprecated"
)

// MCPServer 归一化 MCP 服务器定义（Claude .mcp.json / Codex .mcp.json / agent-plugins mcp.json 的并集）。
// 字符串字段保留 ${PLUGIN_ROOT} / ${CLAUDE_PLUGIN_ROOT} / ${user_config.X} 等原文，
// 展开发生在激活时（需要运行期路径与用户配置），不在解析期。
type MCPServer struct {
	Name          string            `json:"name"`
	Type          string            `json:"type"`
	Command       string            `json:"command,omitempty"`
	Args          []string          `json:"args,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	Cwd           string            `json:"cwd,omitempty"`
	URL           string            `json:"url,omitempty"`
	Headers       map[string]string `json:"headers,omitempty"`
	HeadersHelper string            `json:"headersHelper,omitempty"`
	OAuth         json.RawMessage   `json:"oauth,omitempty"`
	// Extra 未归一化的厂商私有字段原样保留（如 Codex 的 startup_timeout_sec），供激活层按需读取。
	Extra  map[string]json.RawMessage `json:"extra,omitempty"`
	Source string                     `json:"source"` // 声明来源文件或 "manifest"
}

// mcpServerWire 解码中间结构；未知字段进入 Extra。
type mcpServerWire struct {
	Type          string            `json:"type"`
	Transport     string            `json:"transport"`
	Command       string            `json:"command"`
	Args          []string          `json:"args"`
	Env           map[string]string `json:"env"`
	Cwd           string            `json:"cwd"`
	URL           string            `json:"url"`
	Headers       map[string]string `json:"headers"`
	HeadersHelper string            `json:"headersHelper"`
	OAuth         json.RawMessage   `json:"oauth"`
}

func knownMCPKeys() map[string]bool {
	return map[string]bool{"type": true, "transport": true, "command": true, "args": true, "env": true,
		"cwd": true, "url": true, "headers": true, "headersHelper": true, "oauth": true}
}

// mcpServerSet 按声明顺序累积服务器；同名后声明覆盖先声明（Claude 合并语义）。
type mcpServerSet struct {
	order   []string
	servers map[string]MCPServer
	// remote 安装层已下载的远程包：URL → 本地路径。
	remote map[string]string
	// bundleUserConfig .mcpb 包内 user_config 选项，并入插件 userConfig。
	bundleUserConfig []UserConfigOption
}

func (s *mcpServerSet) put(srv MCPServer) {
	if s.servers == nil {
		s.servers = map[string]MCPServer{}
	}
	if _, exists := s.servers[srv.Name]; !exists {
		s.order = append(s.order, srv.Name)
	}
	s.servers[srv.Name] = srv
}

func (s *mcpServerSet) list() []MCPServer {
	out := make([]MCPServer, 0, len(s.order))
	for _, n := range s.order {
		out = append(out, s.servers[n])
	}
	return out
}

// parseMCPFile 读取 MCP 配置文件：{"mcpServers": {...}} 或直接的服务器映射（Claude 允许后者）。
func parseMCPFile(path string, set *mcpServerSet, ds *diagnostics) {
	raw, err := os.ReadFile(path)
	if err != nil {
		ds.errorf("mcp", path, RuleMCPParse, "read failed: %v", err)
		return
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		ds.errorf("mcp", path, RuleMCPParse, "invalid JSON: %v", err)
		return
	}
	servers := top
	if inner, ok := top["mcpServers"]; ok {
		servers = nil
		if err := json.Unmarshal(inner, &servers); err != nil {
			ds.errorf("mcp", path, RuleMCPParse, "mcpServers must be an object: %v", err)
			return
		}
	} else {
		delete(servers, "$schema")
	}
	parseMCPServerMap(servers, path, set, ds)
}

// parseMCPServerMap 解析 name→定义 映射；单个服务器出错只跳过该服务器。
func parseMCPServerMap(servers map[string]json.RawMessage, source string, set *mcpServerSet, ds *diagnostics) {
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	// JSON 对象无序；按名称排序保证同一包多次解析结果稳定（mcp_servers 行 ID 由名称派生）。
	sort.Strings(names)
	for _, name := range names {
		if srv, ok := parseMCPServer(name, servers[name], source, ds); ok {
			set.put(srv)
		}
	}
}

func parseMCPServer(name string, raw json.RawMessage, source string, ds *diagnostics) (MCPServer, bool) {
	var w mcpServerWire
	if strings.TrimSpace(name) == "" {
		ds.errorf("mcp", source, RuleMCPParse, "server name must not be empty")
		return MCPServer{}, false
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		ds.errorf("mcp", source, RuleMCPParse, "server %q: %v", name, err)
		return MCPServer{}, false
	}
	srv := MCPServer{Name: name, Command: w.Command, Args: w.Args, Env: w.Env, Cwd: w.Cwd, URL: w.URL,
		Headers: w.Headers, HeadersHelper: w.HeadersHelper, OAuth: w.OAuth, Source: source}
	srv.Extra = extraFields(raw, knownMCPKeys())
	srv.Type = normalizeMCPType(firstNonEmpty(w.Type, w.Transport), w.Command, w.URL)
	return srv, validateMCPServer(srv, source, ds)
}

func normalizeMCPType(declared, command, rawURL string) string {
	switch strings.ToLower(strings.TrimSpace(declared)) {
	case "stdio":
		return MCPTypeStdio
	case "http", "streamable-http", "streamable_http":
		return MCPTypeHTTP
	case "sse":
		return MCPTypeSSE
	case "ws", "websocket":
		return MCPTypeWS
	case "":
		if command != "" {
			return MCPTypeStdio
		}
		if rawURL != "" {
			return MCPTypeHTTP
		}
	}
	return strings.ToLower(strings.TrimSpace(declared))
}

func validateMCPServer(srv MCPServer, source string, ds *diagnostics) bool {
	switch srv.Type {
	case MCPTypeStdio:
		if strings.TrimSpace(srv.Command) == "" {
			ds.errorf("mcp", source, RuleMCPCommand, "server %q: stdio requires command", srv.Name)
			return false
		}
		return true
	case MCPTypeHTTP, MCPTypeSSE, MCPTypeWS:
		return validateRemoteURL(srv, source, ds)
	default:
		ds.errorf("mcp", source, RuleMCPType, "server %q: unsupported type %q", srv.Name, srv.Type)
		return false
	}
}

func validateRemoteURL(srv MCPServer, source string, ds *diagnostics) bool {
	u, err := url.Parse(srv.URL)
	// URL 可含 ${...} 占位（Claude 允许在 url 中引用插件变量），先按字面校验 scheme。
	if err != nil || u.Scheme == "" || u.Host == "" {
		ds.errorf("mcp", source, RuleMCPURL, "server %q: url must be absolute, got %q", srv.Name, srv.URL)
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	allowed := map[string]bool{"http": true, "https": true, "ws": true, "wss": true}
	if !allowed[scheme] {
		ds.errorf("mcp", source, RuleMCPURL, "server %q: unsupported url scheme %q", srv.Name, scheme)
		return false
	}
	if (scheme == "http" || scheme == "ws") && !isLoopbackHost(u.Hostname()) {
		ds.warnf("mcp", source, RuleMCPHTTPS, "server %q: non-loopback endpoint should use TLS", srv.Name)
	}
	if srv.Type == MCPTypeSSE {
		ds.warnf("mcp", source, RuleMCPSSELegacy, "server %q: HTTP+SSE transport is deprecated (MCP 2026-07-28)", srv.Name)
	}
	return true
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func extraFields(raw json.RawMessage, known map[string]bool) map[string]json.RawMessage {
	var all map[string]json.RawMessage
	if json.Unmarshal(raw, &all) != nil {
		return nil
	}
	var extra map[string]json.RawMessage
	for k, v := range all {
		if known[k] {
			continue
		}
		if extra == nil {
			extra = map[string]json.RawMessage{}
		}
		extra[k] = v
	}
	return extra
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
