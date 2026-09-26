package mcp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strings"
)

// httpTokenPattern RFC 9110 field-name token（x-mcp-header 名称约束）。
var httpTokenPattern = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// maxSafeInteger JavaScript 安全整数范围（x-mcp-header 整数值约束）。
const maxSafeInteger = 1<<53 - 1

// headerParam tool inputSchema 中 x-mcp-header 标注：属性路径 → Mcp-Param-{Name}。
type headerParam struct {
	Name string
	Path []string
}

// toolHeaderParams 解析并校验工具的 x-mcp-header 标注（2026-07-28 streamable-http §Custom Headers）。
// 标注只能出现在从根经纯 properties 链可达、类型为 string/integer/boolean 的属性上，名称为 HTTP token
// 且大小写不敏感唯一；任一违规则整个工具定义无效（返回 ok=false，调用方须从工具列表中排除）。
func toolHeaderParams(schema any) ([]headerParam, bool) {
	w := &headerWalker{seen: map[string]bool{}, valid: true}
	w.walk(schema, nil, true)
	return w.out, w.valid
}

type headerWalker struct {
	out   []headerParam
	seen  map[string]bool
	valid bool
}

func (w *headerWalker) walk(node any, path []string, reachable bool) {
	switch n := node.(type) {
	case []any:
		for _, it := range n {
			w.walk(it, nil, false)
		}
	case map[string]any:
		if name, has := n["x-mcp-header"]; has {
			w.annotate(n, name, path, reachable)
		}
		for k, v := range n {
			if props, ok := v.(map[string]any); ok && k == "properties" {
				for pk, pv := range props {
					w.walk(pv, append(append([]string(nil), path...), pk), reachable)
				}
				continue
			}
			w.walk(v, nil, false) // 经 items / 组合 / 条件 / $ref 等到达的标注一律无效
		}
	}
}

func (w *headerWalker) annotate(obj map[string]any, name any, path []string, reachable bool) {
	n, _ := name.(string)
	t, _ := obj["type"].(string)
	typeOK := t == "string" || t == "integer" || t == "boolean"
	if !reachable || len(path) == 0 || !httpTokenPattern.MatchString(n) || w.seen[strings.ToLower(n)] || !typeOK {
		w.valid = false
		return
	}
	w.seen[strings.ToLower(n)] = true
	w.out = append(w.out, headerParam{Name: n, Path: append([]string(nil), path...)})
}

// encodeHeaderValue 值编码：可见 ASCII 且无首尾空白、且不形似哨兵时原样，否则 =?base64?…?=。
func encodeHeaderValue(v string) string {
	sentinel := strings.HasPrefix(v, "=?base64?") && strings.HasSuffix(v, "?=")
	safe := v != "" && strings.TrimSpace(v) == v && !sentinel
	for _, r := range v {
		if (r < 0x20 || r > 0x7e) && r != '\t' {
			safe = false
			break
		}
	}
	if safe {
		return v
	}
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(v)) + "?="
}

// headerValueOf 参数值 → 头部字符串；null / 缺失 / 非原始类型 → 省略。
func headerValueOf(args map[string]any, path []string) (string, bool) {
	var cur any = args
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		if cur, ok = m[p]; !ok {
			return "", false
		}
	}
	switch v := cur.(type) {
	case string:
		return v, true
	case bool:
		return fmt.Sprint(v), true
	case float64:
		if v != math.Trunc(v) || math.Abs(v) > maxSafeInteger {
			return "", false
		}
		return fmt.Sprintf("%d", int64(v)), true
	case json.Number:
		return v.String(), true
	}
	return "", false
}

// setRequestHeaders MCP-Protocol-Version（两个纪元）；新纪元追加 Mcp-Method / Mcp-Name / Mcp-Param-*；
// 旧纪元回传 Mcp-Session-Id。配置头先写，协议头后写：配置不能改写协议必需头。
func (c *MCPClient) setRequestHeaders(req *http.Request, rpc mcpRPCRequest) {
	c.setConfiguredHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	version := c.protocolVersion()
	if rpc.Method == "server/discover" {
		version = modernProtocolVersion
	}
	req.Header.Set("MCP-Protocol-Version", version)
	if version != modernProtocolVersion {
		if sid := c.legacySession.Load(); sid != nil && *sid != "" {
			req.Header.Set("Mcp-Session-Id", *sid)
		}
		return
	}
	if rpc.Method == "" {
		return
	}
	req.Header.Set("Mcp-Method", rpc.Method)
	params, _ := rpc.Params.(map[string]any)
	switch rpc.Method {
	case "tools/call", "prompts/get":
		if name, ok := params["name"].(string); ok {
			req.Header.Set("Mcp-Name", encodeHeaderValue(name))
		}
	case "resources/read":
		if uri, ok := params["uri"].(string); ok {
			req.Header.Set("Mcp-Name", encodeHeaderValue(uri))
		}
	}
	if rpc.Method == "tools/call" {
		c.setParamHeaders(req, params)
	}
}

func (c *MCPClient) setParamHeaders(req *http.Request, params map[string]any) {
	name, _ := params["name"].(string)
	args, _ := params["arguments"].(map[string]any)
	hp, ok := c.toolHeaders.Load(name)
	if !ok || args == nil {
		return
	}
	for _, h := range hp.([]headerParam) {
		if v, ok := headerValueOf(args, h.Path); ok {
			req.Header.Set("Mcp-Param-"+h.Name, encodeHeaderValue(v))
		}
	}
}

// filterHeaderAnnotatedTools Streamable HTTP 客户端必须排除 x-mcp-header 标注无效的工具，并缓存
// 有效标注供 tools/call 使用；其他传输可忽略标注。
func (c *MCPClient) filterHeaderAnnotatedTools(tools []MCPTool) []MCPTool {
	if c.cfg.Transport != MCPStreamableHTTP {
		return tools
	}
	out := tools[:0]
	for _, t := range tools {
		var schema any
		if len(t.InputSchema) > 0 {
			_ = json.Unmarshal(t.InputSchema, &schema) //nolint:errcheck // 非法 schema 由 registerTools 另行处理
		}
		params, ok := toolHeaderParams(schema)
		if !ok {
			slog.Warn("mcp: tool rejected, invalid x-mcp-header annotation", "server", c.cfg.ServerName, "tool", t.Name)
			continue
		}
		c.toolHeaders.Store(t.Name, params)
		out = append(out, t)
	}
	return out
}
