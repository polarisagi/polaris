package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// CallTool 直接路由调用指定的 MCP 工具。
// 执行 PolicyGate 校验（与 InMemoryToolRegistry.ExecuteTool 语义一致）。
func (m *MCPManager) CallTool(ctx context.Context, serverID, toolName string, args map[string]any) (string, error) {
	m.mu.RLock()
	e, ok := m.entries[serverID]
	m.mu.RUnlock()
	if !ok {
		return "", apperr.New(apperr.CodeInternal, "mcp_manager: server not found: "+serverID)
	}

	llmName := MCPToolName(e.name, toolName)
	argsBytes, _ := json.Marshal(args)

	m.mu.RLock()
	env := m.envelope
	m.mu.RUnlock()

	if env == nil {
		return "", apperr.New(apperr.CodeInternal, "mcp_manager: envelope not initialized")
	}

	res, err := env.Execute(ctx, sandbox.ExecRequest{
		Principal:  sandbox.PrincipalAgent,
		Kind:       sandbox.KindToolExecute,
		Resource:   llmName,
		TrustTier:  types.TrustTier(e.cfg.TrustTier),
		Tool:       types.Tool{Name: llmName, Source: types.ToolMCP, TrustTier: types.TrustTier(e.cfg.TrustTier)},
		Input:      argsBytes,
		TaintLevel: types.TaintMedium,
	})
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "mcp_manager: call tool", err)
	}
	if !res.Success {
		return "", apperr.New(apperr.CodeInternal, "MCPManager.CallTool failed: "+res.Error)
	}
	return string(res.Output), nil
}

// registerTools 注册合法的 MCP 工具到 sandbox，返回实际注册成功的工具子集。
// 服务器名（serverName）在 Add() 中已经过 validateLLMNamePart 校验，此处信任。
// 工具名来自外部 MCP 服务器，不可控：非法字符静默替换并记录警告；超长则跳过。
// serverID 是 entries map 键（DB 实例 ID），供工具执行期 OAuth 挑战定位对应 entry。
func (m *MCPManager) registerTools(serverID, serverName string, client *MCPClient, tools []MCPTool) []MCPTool {
	// 确定此 server 的污点等级：白名单 → TaintMedium；其余 → TaintHigh
	taint := types.TaintHigh
	if client.cfg.Trusted {
		taint = types.TaintMedium
	}

	// 计算超时时间：默认 5 分钟 (300s)，如果有配置则使用配置值
	toolTimeout := client.cfg.Timeout
	if toolTimeout <= 0 {
		toolTimeout = 5 * time.Minute
	}

	// 注册前安全扫描（prompt injection 检测）
	// Deny 级工具直接跳过；HITL/Warn 级仍注册但已记录日志（可后续接入 HITL 网关）
	scanner := NewToolSecurityScanner()
	deniedTools := make(map[string]bool)
	for _, scanResult := range scanner.ScanAll(tools, ScanRiskDeny) {
		deniedTools[scanResult.ToolName] = true
		slog.Error("mcp: tool blocked by security scanner",
			"server", serverName, "tool", scanResult.ToolName, "reasons", scanResult.Reasons)
	}
	// HITL 扫描（记录，不阻断）
	_ = scanner.ScanAll(tools, ScanRiskHITL)

	valid := make([]MCPTool, 0, len(tools))
	for _, t := range tools {
		if deniedTools[t.Name] {
			continue
		}
		llmName := MCPToolName(serverName, t.Name)
		if llmName != MCPToolName(serverName, SanitizeToolNamePart(t.Name)) || t.Name != SanitizeToolNamePart(t.Name) {
			slog.Warn("mcp: tool name sanitized", "server", serverName, "original", t.Name, "llm_name", llmName)
		}
		if len(llmName) > maxLLMToolNameLen {
			slog.Warn("mcp: tool LLM name too long, skipped", "server", serverName, "tool", t.Name, "llm_name", llmName, "max", maxLLMToolNameLen)
			continue
		}
		fn := makeMCPToolFn(m, serverID, serverName, client, t.Name)
		// RegisterRich 无条件注册到进程内沙箱执行表：MCP Apps app-only 工具
		// （visibility 不含 "model"）虽不进模型工具目录，但仍须经 View RPC 走
		// 同一 ExecuteTool 入口调用（HE-3，见 CallToolAsApp）。
		m.sandbox.RegisterRich(llmName, fn, taint)

		// modelVisible=false（visibility 显式排除 "model"）时不同步到
		// InMemoryToolRegistry / Catalog——两者都是"模型可见工具目录"的数据源，
		// 纳入 app-only 工具会使其出现在 LLM 的工具列表里（apps_spec.mdx
		// §Visibility "tools/list behavior"：host MUST NOT 将其纳入）。
		modelVisible := ToolUIVisibleTo(t.UI, "model")
		if modelVisible {
			m.registerModelVisibleTool(serverID, client, serverName, llmName, t, taint, toolTimeout)
		} else {
			m.registerAppOnlyTool(client, serverName, llmName, t, toolTimeout)
		}

		valid = append(valid, t)
	}
	return valid
}

// makeMCPToolFn 创建调用 MCP 工具的富执行函数。
// 返回完整 ToolResult（含 ImageParts），使用 CallToolTainted 进行污点保护反序列化（M07 §1 安全要求）。
func makeMCPToolFn(m *MCPManager, serverID, serverName string, client *MCPClient, mcpName string) sandbox.InProcessRichFn {
	return func(ctx context.Context, spec sandbox.SandboxSpec) (*types.ToolResult, error) {
		var args map[string]any
		if len(spec.Input) > 0 {
			if err := json.Unmarshal(spec.Input, &args); err != nil {
				return nil, apperr.New(apperr.CodeInvalidInput, "mcp: invalid tool input JSON: "+err.Error())
			}
		}
		// CallToolTainted 内部执行 TaintPreservingDecoder 对响应逐叶打标取最高值；
		// RegisterRich 的 taint 参数是注册时的静态服务器级污点（供策略预判），
		// 与此处按次调用实际测得的污点是两回事，此前被 `_` 丢弃，导致
		// agent_execute_dag.go 的 GlobalTaintLevel/hasHighTaint 逻辑对 MCP 工具结果
		// 永远视为 TaintNone——外部/不可信响应内容完全没有参与污点升级判断。
		callCtx, cancel := context.WithTimeout(ctx, client.cfg.Timeout)
		defer cancel()
		text, imgs, raw, taintLevel, err := client.CallToolTaintedRaw(callCtx, mcpName, args)
		if err != nil {
			// OAuth 挑战（401 刷新已失败 / 403 insufficient_scope）：置 AuthRequired，
			// 返回面向 LLM 的可读错误，而不是透传底层传输错误（basic_authorization.md）。
			if authErr := m.authRequiredError(serverID, serverName, err); authErr != nil {
				return nil, authErr
			}
			return nil, apperr.Wrap(apperr.CodeInternal, "makeMCPToolFn", err)
		}
		return &types.ToolResult{
			Success:    true,
			Output:     []byte(text),
			ImageParts: imgs, // MCP type="image" content block 解析结果
			TaintLevel: taintLevel,
			// MCPRaw 原始 CallToolResult JSON：不改变上方 Output（模型看到的文本）本身，
			// 只在结果对象上附加一份原文，供 agent_execute_dag.go 判定该工具是否关联
			// MCP Apps UI 视图时透传给前端宿主（M8f-1）。
			MCPRaw: raw,
		}, nil
	}
}

// asyncToolSuffix 异步变体的 LLM 工具名后缀（GD-08-001，M13-bis §8.4）。
const asyncToolSuffix = "_async"

// makeMCPToolAsyncFn 创建 MCP 工具的异步变体执行函数：立即返回
// {"task_id":"...","status":"pending"}，不等待实际 MCP 调用完成。
// 真正的调用结果通过 m.runAsyncCall 派生的后台 goroutine 写入 tasks_cache，
// LLM 侧用 get_task_result 工具轮询。
func makeMCPToolAsyncFn(m *MCPManager, serverID, serverName string, client *MCPClient, mcpName string) sandbox.InProcessRichFn {
	return func(ctx context.Context, spec sandbox.SandboxSpec) (*types.ToolResult, error) {
		var args map[string]any
		if len(spec.Input) > 0 {
			if err := json.Unmarshal(spec.Input, &args); err != nil {
				return nil, apperr.New(apperr.CodeInvalidInput, "mcp: invalid tool input JSON: "+err.Error())
			}
		}
		taskID := m.runAsyncCall(ctx, serverID, serverName, client, mcpName, args)
		out, _ := json.Marshal(map[string]string{"task_id": taskID, "status": string(AsyncTaskPending)}) //nolint:errchkjson // 固定字段结构体，Marshal 不会失败
		return &types.ToolResult{Success: true, Output: out}, nil
	}
}

// registryTool 注册表中的工具定义（模型可见与 app-only 共用同一风险/信任口径）。
func registryTool(client *MCPClient, llmName string, t MCPTool, toolTimeout time.Duration) types.Tool {
	riskLevel := types.RiskHigh
	if client.cfg.Trusted {
		riskLevel = types.RiskMedium
	}
	return types.Tool{
		Name:        llmName,
		Description: t.Description,
		InputSchema: t.InputSchema,
		Source:      types.ToolMCP,
		RiskLevel:   riskLevel,
		TrustTier:   types.TrustTier(client.cfg.TrustTier),
		Timeout:     toolTimeout,
	}
}

// registerAppOnlyTool app-only 工具只进注册表的隐藏集合（不进 Catalog、不注册异步变体）：
// 模型看不到也调不到，View 调用经 ExecuteAppTool 走同一执行体。
func (m *MCPManager) registerAppOnlyTool(client *MCPClient, serverName, llmName string, t MCPTool, toolTimeout time.Duration) {
	slog.Info("mcp: tool is app-only, excluded from model tool directory", "server", serverName, "tool", t.Name)
	if m.toolReg == nil {
		return
	}
	if err := m.toolReg.RegisterAppOnly(registryTool(client, llmName, t, toolTimeout)); err != nil {
		slog.Warn("mcp: failed to register app-only tool", "server", serverName, "tool", llmName, "err", err)
	}
}

// registerModelVisibleTool 同步一个模型可见工具（visibility 含 "model"）到
// InMemoryToolRegistry + 统一 Catalog，并注册其 GD-08-001 异步变体。从
// registerTools 拆出：app-only 工具跳过本函数整体（≤60 行治理 + 职责分离）。
func (m *MCPManager) registerModelVisibleTool(serverID string, client *MCPClient, serverName, llmName string, t MCPTool, taint types.TaintLevel, toolTimeout time.Duration) {
	if m.toolReg != nil {
		if regErr := m.toolReg.Register(registryTool(client, llmName, t, toolTimeout)); regErr != nil {
			slog.Warn("mcp: failed to sync tool to InMemoryToolRegistry", "server", serverName, "tool", llmName, "err", regErr)
		}
	}

	var resourceURI string
	var visibility []string
	if t.UI != nil {
		resourceURI, visibility = t.UI.ResourceURI, t.UI.Visibility
	}
	if m.catalog != nil {
		m.catalog.Register(protocol.CatalogEntry{
			Name:        llmName,
			Description: t.Description,
			Parameters:  t.InputSchema,
			Source:      types.ToolMCP,
			TrustTier:   types.TrustTier(client.cfg.TrustTier),
			TaintLevel:  taint,
			Timeout:     toolTimeout,
			MCPServerID: client.cfg.ServerName,
			MCPToolName: t.Name,
			ResourceURI: resourceURI,
			Visibility:  visibility,
		})
	}

	// GD-08-001: 注册对应的异步变体（M13-bis §8.4），LLM 对预估耗时较长的调用
	// 可主动选择 *_async 变体立即拿回 task_id，再用 get_task_result 轮询，
	// 避免同步阻塞。异步变体名超长时静默跳过（不影响同步变体本身可用性）。
	asyncLLMName := llmName + asyncToolSuffix
	if len(asyncLLMName) > maxLLMToolNameLen {
		slog.Warn("mcp: async tool LLM name too long, async variant skipped", "server", serverName, "tool", t.Name, "llm_name", asyncLLMName, "max", maxLLMToolNameLen)
		return
	}
	asyncFn := makeMCPToolAsyncFn(m, serverID, serverName, client, t.Name)
	m.sandbox.RegisterRich(asyncLLMName, asyncFn, taint)
	if m.catalog != nil {
		m.catalog.Register(protocol.CatalogEntry{
			Name: asyncLLMName,
			Description: "[async variant] " + t.Description +
				" Returns {task_id, status:pending} immediately; poll with get_task_result.",
			Parameters:  t.InputSchema,
			Source:      types.ToolMCP,
			TrustTier:   types.TrustTier(client.cfg.TrustTier),
			TaintLevel:  taint,
			Timeout:     toolTimeout,
			MCPServerID: client.cfg.ServerName,
			MCPToolName: t.Name,
		})
	}
}

func (m *MCPManager) unregisterTools(serverName string, tools []MCPTool) {
	for _, t := range tools {
		llmName := MCPToolName(serverName, t.Name)
		m.sandbox.Unregister(llmName)
		// 同步从 InMemoryToolRegistry 注销，保持可发现性状态一致
		if m.toolReg != nil {
			m.toolReg.Unregister(llmName)
		}
		// 同步从 unified catalog 注销
		if m.catalog != nil {
			m.catalog.Unregister(llmName)
		}

		// GD-08-001: 同步注销异步变体
		asyncLLMName := llmName + asyncToolSuffix
		m.sandbox.Unregister(asyncLLMName)
		if m.catalog != nil {
			m.catalog.Unregister(asyncLLMName)
		}
	}
}

// MCPToolName 生成 LLM 工具名，格式：mcp__<serverName>__<toolName>。
// serverName 由调用方（Add）保证合法；toolName 来自外部，经 SanitizeToolNamePart 处理。
func (m *MCPManager) MCPToolName(serverName, toolName string) string {
	return MCPToolName(serverName, toolName)
}

func MCPToolName(serverName, toolName string) string {
	return "mcp__" + serverName + "__" + SanitizeToolNamePart(toolName)
}

// SanitizeToolNamePart 将外部工具名中不符合 ^[a-zA-Z0-9_-]+$ 的字符替换为下划线。
// 仅用于来自外部 MCP 服务器的工具名；用户配置的服务器名走 validateLLMNamePart 硬校验。
func SanitizeToolNamePart(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, s)
}

// validateLLMNamePart 校验字符串是否满足 OpenAI 工具名规范 ^[a-zA-Z0-9_-]+$。
// 用于用户可控的名称（MCP server name、skill name），非法则快速失败。
func validateLLMNamePart(s string) error {
	if s == "" {
		return apperr.New(apperr.CodeInvalidInput, "name must not be empty")
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("char %q not in ^[a-zA-Z0-9_-]+$", r))
		}
	}
	return nil
}

// IsValidLLMName 导出版本：检查完整工具名（含前缀）是否满足 ^[a-zA-Z0-9_-]+$。
// 供 sysadmin.BuildToolSchemas 等外部包做防御性过滤使用。
func IsValidLLMName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

// DynamicConnectRequest 动态连接 MCP server 的参数。
type DynamicConnectRequest struct {
	ServerName string // 唯一名称，用于工具名前缀
	Transport  string // "stdio" | "sse" | "http"
	Command    string // stdio 模式：可执行文件路径
	Args       []string
	URL        string // sse/http 模式：端点 URL
	TrustTier  int    // TrustTier
}
