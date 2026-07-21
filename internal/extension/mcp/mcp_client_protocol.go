package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── MCP 协议方法 ─────────────────────────────────────────────────────────────

// mcpProtocolVersion 当前实现支持的 MCP 协议版本（2025-11-25 为当前稳定版本）。
const mcpProtocolVersion = "2025-11-25"

// Initialize 执行 MCP 初始化握手，校验服务器返回的协议版本。
func (c *MCPClient) Initialize(ctx context.Context) error {
	caps := map[string]any{}
	if c.serverReqHandler != nil {
		caps["roots"] = map[string]any{"listChanged": false}
		caps["sampling"] = map[string]any{}
	}
	result, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    caps,
		"clientInfo":      map[string]any{"name": "polaris", "version": "1.0"},
	})
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mcp: initialize", err)
	}
	// 校验服务器返回的协议版本（规范要求：不支持则应断连）
	var initResp struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(result, &initResp) == nil && initResp.ProtocolVersion != "" {
		if initResp.ProtocolVersion != mcpProtocolVersion {
			slog.Warn("mcp: server protocol version mismatch",
				"server", initResp.ProtocolVersion, "client", mcpProtocolVersion)
			// 仅警告不中断：允许向下兼容旧版服务器（2024-11-05）
		}
	}
	return c.notify(ctx, "notifications/initialized", nil)
}

// mcpContentBlock MCP 工具响应的 content block。
// MCP spec 定义两种主要类型：
//   - type="text": {type, text}
//   - type="image": {type, data (base64), mimeType}
//
// 参考：MCP 2025-11-25 §Tools/CallTool Response
type mcpContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`     // image block: base64 编码的图片数据
	MIMEType string `json:"mimeType,omitempty"` // image block: 如 "image/jpeg"
}

// parseMCPContent 解析 MCP content block 列表，分离文本和图片。
// image block 的 base64 data 解码为原始字节构造 types.ImagePart。
// 解码失败的 image block 记录警告日志后跳过，不中断流程。
func parseMCPContent(blocks []mcpContentBlock) (text string, imgs []types.ImagePart) {
	var sb strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "text":
			sb.WriteString(b.Text)
		case "image":
			if b.Data == "" || b.MIMEType == "" {
				slog.Warn("mcp: image content block missing data or mimeType, skipping")
				continue
			}
			// base64 → 原始字节（ImagePart.Data 约定为 raw bytes，非 base64）
			raw, err := decodeBase64(b.Data)
			if err != nil {
				slog.Warn("mcp: failed to decode image content block", "err", err)
				continue
			}
			imgs = append(imgs, types.ImagePart{
				Type:      "image",
				MediaType: b.MIMEType,
				Data:      raw,
			})
		default:
			// 未知类型（embedded_resource 等）静默跳过，保持向前兼容
			slog.Debug("mcp: unknown content block type, skipping", "type", b.Type)
		}
	}
	return sb.String(), imgs
}

// decodeBase64 尝试标准 base64 解码，失败时回退 URL-safe 变体。
// MCP 服务器实现差异：部分使用标准 +/（StdEncoding），部分使用 URL-safe -_（RawURLEncoding）。
func decodeBase64(s string) ([]byte, error) {
	// 先尝试标准编码（含 padding）
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil {
		return raw, nil
	}
	// 再尝试 URL-safe 无 padding 编码（RFC 4648 §5）
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "mcp: decodeBase64", err)
	}
	return raw, nil
}

// ListTools 查询服务端工具列表。
func (c *MCPClient) ListTools(ctx context.Context) ([]MCPTool, error) {
	result, err := c.call(ctx, "tools/list", nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mcp: tools/list", err)
	}
	var resp struct {
		Tools []MCPTool `json:"tools"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp: tools/list parse: %v", err), err)
	}
	return resp.Tools, nil
}

// MCPResource 表示 MCP resources/list 返回的一条资源引用（MCP 2025-11-25 规范
// §Resources/ListResources）。2026-07-21 deadcode 审查补齐：此前
// knowledge/connector.MCPKnowledgeConnector.List/Fetch 是自承的桩实现，本方法
// 是缺失的真实桥接——与既有 ListTools/CallTool 同一 c.call() RPC 调用方式，
// 只是换了 MCP 协议里"资源"能力对应的方法名。
type MCPResource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MIMEType    string `json:"mimeType,omitempty"`
}

// ResourcesList 查询服务端资源列表（MCP resources/list）。
func (c *MCPClient) ResourcesList(ctx context.Context) ([]MCPResource, error) {
	result, err := c.call(ctx, "resources/list", nil)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mcp: resources/list", err)
	}
	var resp struct {
		Resources []MCPResource `json:"resources"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp: resources/list parse: %v", err), err)
	}
	return resp.Resources, nil
}

// MCPResourceContent 表示 resources/read 返回的单条内容块。MCP spec 里文本资源
// 用 text 字段，二进制资源用 blob 字段（base64），两者互斥。
type MCPResourceContent struct {
	URI      string `json:"uri"`
	MIMEType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// ResourcesRead 读取指定 URI 的资源内容（MCP resources/read）。
func (c *MCPClient) ResourcesRead(ctx context.Context, uri string) ([]MCPResourceContent, error) {
	result, err := c.call(ctx, "resources/read", map[string]any{"uri": uri})
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp: resources/read %q", uri), err)
	}
	var resp struct {
		Contents []MCPResourceContent `json:"contents"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp: resources/read parse: %v", err), err)
	}
	return resp.Contents, nil
}

// CallTool 调用指定工具并返回文本和图片结果。
func (c *MCPClient) CallTool(ctx context.Context, name string, arguments map[string]any) (string, []types.ImagePart, error) {
	result, err := c.call(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": arguments,
	})
	if err != nil {
		return "", nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp: tools/call %q", name), err)
	}
	var resp struct {
		Content []mcpContentBlock `json:"content"`
		IsError bool              `json:"isError"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return "", nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp: tools/call parse: %v", err), err)
	}
	text, imgs := parseMCPContent(resp.Content)
	if resp.IsError {
		return "", nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("mcp: tool error: %s", text))
	}
	return text, imgs, nil
}

// CallToolTainted 调用工具，对响应 JSON 进行污点保护反序列化，返回内容、图片、最高污点等级。
//
// 依赖 TaintPreservingDecoder 对所有 string 叶子打标（M07 §1 安全要求）。
// trusted 由 MCPClientConfig.Trusted 决定：白名单 → TaintMedium；其余 → TaintHigh。
func (c *MCPClient) CallToolTainted(ctx context.Context, name string, arguments map[string]any) (string, []types.ImagePart, types.TaintLevel, error) {
	result, err := c.call(ctx, "tools/call", map[string]any{
		"name":      name,
		"arguments": arguments,
	})
	if err != nil {
		return "", nil, types.TaintHigh, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp: tools/call %q", name), err)
	}

	// 污点保护反序列化：遍历 JSON 树，对所有 string 叶子打标
	dec := NewTaintPreservingDecoder(c.cfg.ServerName, c.cfg.Trusted)
	node := dec.Decode(result, "")
	maxTaint := node.MaxTaint()
	if maxTaint < dec.Taint() {
		// 若 JSON 全为非 string 节点（无叶子字符串），仍保守取 server 级别
		maxTaint = dec.Taint()
	}

	var resp struct {
		Content []mcpContentBlock `json:"content"`
		IsError bool              `json:"isError"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return "", nil, maxTaint, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp: tools/call parse: %v", err), err)
	}
	text, imgs := parseMCPContent(resp.Content)
	if resp.IsError {
		return "", nil, maxTaint, apperr.New(apperr.CodeInternal, fmt.Sprintf("mcp: tool error: %s", text))
	}
	return text, imgs, maxTaint, nil
}

// Close 关闭连接并释放资源。
func (c *MCPClient) Close() {
	c.once.Do(func() {
		close(c.done)
		if c.stdin != nil {
			c.stdin.Close()
		}
		if c.cmd != nil {
			// 先显式 Kill 再 Wait，防止子进程僵尸（exec.Command 不自动回收）
			if c.cmd.Process != nil {
				_ = c.cmd.Process.Kill()
			}
			if err := c.cmd.Wait(); err != nil {
				slog.Warn("mcp: server process exited", "server", c.cfg.ServerName,
					"exit_code", c.cmd.ProcessState.ExitCode(), "err", err)
			}
		}
	})
}
