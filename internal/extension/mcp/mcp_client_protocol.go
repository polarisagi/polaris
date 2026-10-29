package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ─── MCP 协议方法 ─────────────────────────────────────────────────────────────

// mcpProtocolVersion 旧纪元回退时使用的版本（2025-11-25；新纪元见 mcp_client_era.go）。
const mcpProtocolVersion = "2025-11-25"

// initializeLegacy 旧纪元（2025-11-25 及更早）initialize 握手，校验服务器返回的协议版本。
// 能力声明与新纪元共用 clientCapabilities（不再硬编码 roots——Polaris 不暴露文件系统
// roots，且 Roots 已弃用；roots/list 仍原地应答空列表，只是不作为声明式能力）。
func (c *MCPClient) initializeLegacy(ctx context.Context) error {
	result, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    c.clientCapabilities(),
		"clientInfo":      map[string]any{"name": "polaris", "version": "1.0"},
	})
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mcp: initialize", err)
	}
	// 校验服务器返回的协议版本（规范要求：不支持则应断连）
	var initResp struct {
		ProtocolVersion string `json:"protocolVersion"`
		Instructions    string `json:"instructions"`
		Capabilities    struct {
			Experimental map[string]json.RawMessage `json:"experimental"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(result, &initResp); err != nil {
		slog.Warn("mcp: unparsable initialize result", "server", c.cfg.ServerName, "err", err)
	}
	c.serverMeta.Store(&ServerMeta{Instructions: initResp.Instructions, Experimental: initResp.Capabilities.Experimental})
	if initResp.ProtocolVersion != "" {
		if initResp.ProtocolVersion != mcpProtocolVersion {
			slog.Warn("mcp: server protocol version mismatch",
				"server", initResp.ProtocolVersion, "client", mcpProtocolVersion)
			// 仅警告不中断：允许向下兼容旧版服务器（2024-11-05）
		}
	}
	c.era.Store(int32(eraLegacy))
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
	// MCP Apps UI 元数据解析（apps_spec.mdx §Resource Discovery）：单一choke point，
	// 所有传输/纪元共用同一份 tools/list 解析结果，见 mcp_apps_tool_meta.go。
	for i := range resp.Tools {
		resp.Tools[i].UI = ParseToolUI(resp.Tools[i].Meta, resp.Tools[i].Name, c.cfg.ServerName)
	}
	return c.filterHeaderAnnotatedTools(resp.Tools), nil
}

// MCPResource 表示 MCP resources/list 返回的一条资源引用（MCP 2025-11-25 规范
// §Resources/ListResources）。2026-07-21 deadcode 审查补齐：此前
// knowledge/connector.MCPKnowledgeConnector.List/Fetch 是自承的桩实现，本方法
// 是缺失的真实桥接——与既有 ListTools/CallTool 同一 c.call() RPC 调用方式，
// 只是换了 MCP 协议里"资源"能力对应的方法名。
type MCPResource = protocol.MCPResource

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
type MCPResourceContent = protocol.MCPResourceContent

// ResourcesRead 读取指定 URI 的资源内容（MCP resources/read）。
// resources/read 是 MRTR §Supported Requests 之一，经 c.request 处理可能的 InputRequiredResult。
func (c *MCPClient) ResourcesRead(ctx context.Context, uri string) ([]MCPResourceContent, error) {
	result, err := c.request(ctx, "resources/read", map[string]any{"uri": uri})
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
// methodToolsCall 是 MRTR §Supported Requests 之一，经 c.request 处理可能的 InputRequiredResult
// （elicitation/sampling 多轮往返）与 HeaderMismatch 重试；resultType 字段不影响下方解析。
func (c *MCPClient) CallTool(ctx context.Context, name string, arguments map[string]any) (string, []types.ImagePart, error) {
	result, err := c.request(ctx, methodToolsCall, map[string]any{
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
// 污点解码作用在 c.request 完成全部 MRTR 轮次后的最终结果上，中间轮次的
// inputRequests/inputResponses 不参与污点计算。
func (c *MCPClient) CallToolTainted(ctx context.Context, name string, arguments map[string]any) (string, []types.ImagePart, types.TaintLevel, error) {
	result, err := c.request(ctx, methodToolsCall, map[string]any{
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

// CallToolTaintedRaw 与 CallToolTainted 等价，额外返回服务器应答的原始
// CallToolResult JSON（result 字段原文：content/structuredContent/_meta/isError）。
//
// 不改 CallToolTainted 本身签名——它是 protocol.MCPClient 接口方法，四处调用点
// （async_tasks.go/mcp_client_tasks.go/knowledge/connector 等）都不需要原始 JSON；
// 本方法只服务 makeMCPToolFn 一个调用点（M8f-1：MCP Apps 工具结果需要把原始
// CallToolResult 透传给前端宿主渲染 UI，是"找最小改动点"的结果，而非扩大公共接口）。
func (c *MCPClient) CallToolTaintedRaw(ctx context.Context, name string, arguments map[string]any) (text string, imgs []types.ImagePart, raw json.RawMessage, taintLevel types.TaintLevel, err error) {
	result, err := c.request(ctx, methodToolsCall, map[string]any{
		"name":      name,
		"arguments": arguments,
	})
	if err != nil {
		return "", nil, nil, types.TaintHigh, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp: tools/call %q", name), err)
	}

	dec := NewTaintPreservingDecoder(c.cfg.ServerName, c.cfg.Trusted)
	node := dec.Decode(result, "")
	maxTaint := node.MaxTaint()
	if maxTaint < dec.Taint() {
		maxTaint = dec.Taint()
	}

	var resp struct {
		Content []mcpContentBlock `json:"content"`
		IsError bool              `json:"isError"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return "", nil, nil, maxTaint, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp: tools/call parse: %v", err), err)
	}
	text, imgs = parseMCPContent(resp.Content)
	if resp.IsError {
		return "", nil, result, maxTaint, apperr.New(apperr.CodeInternal, fmt.Sprintf("mcp: tool error: %s", text))
	}
	return text, imgs, result, maxTaint, nil
}

// Close 关闭连接并释放资源。
func (c *MCPClient) Close() {
	c.once.Do(func() {
		// stdio 传输下，subscriptions/listen 的响应只在服务器优雅关闭订阅时才会到来
		// （spec_subscriptions.md §Cancellation）；客户端主动关闭必须显式发送
		// notifications/cancelled 告知服务器，否则服务器会一直以为订阅还活着。必须
		// 在 close(c.done)/stdin.Close() 之前发送——两者任一发生后 c.notify 都发不出去了。
		if c.cfg.Transport == MCPStdio {
			if id := c.activeSubscriptionID.Load(); id != 0 {
				cancelCtx, cancel := context.WithTimeout(context.Background(), subscriptionCancelNotifyTimeout)
				if err := c.notify(cancelCtx, notificationCancelled, map[string]any{"requestId": id}); err != nil {
					slog.Warn("mcp: send notifications/cancelled failed", "server", c.cfg.ServerName, "err", err)
				}
				cancel()
			}
		}
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
