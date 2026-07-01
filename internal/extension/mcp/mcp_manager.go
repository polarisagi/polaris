package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// MCPServerInfo protocol.MCPServerInfo 本地别名，使包内调用无需显式引用 protocol 包。
type MCPServerInfo = protocol.MCPServerInfo

type mcpEntry struct {
	client *MCPClient
	name   string
	cfg    MCPClientConfig
	tools  []MCPTool
	errMsg string
}

// ToolRegistrar MCP 工具注册到 InMemoryToolRegistry 的最小接口（consumer-side 定义，防包循环）。
// 实现由 pkg/action/tool.InMemoryToolRegistry 提供。
type ToolRegistrar interface {
	Register(tool types.Tool) error
	Unregister(name string)
}

// NetApprovalStore MCP 网络访问审批持久化存储（consumer-side 定义）。
// 实现由 internal/store/repo.SystemRepository 提供（preferences 表）。
// key 格式：mcp.net.approved.<server_id>，value："approved" | "denied"；缺失=待审批(pending)。
type NetApprovalStore interface {
	GetPreference(ctx context.Context, key string) (string, error)
	UpsertPreference(ctx context.Context, key, value string) error
}

// SandboxToolRegistrar 向进程内沙箱注册/注销工具的最小接口（consumer-side 定义，防具体类型耦合）。
// 实现由 internal/sandbox.InProcessSandbox 提供；接口定义在此（调用方），符合 Go consumer-side 原则。
type SandboxToolRegistrar interface {
	Register(toolName string, fn sandbox.InProcessFn)
	RegisterWithTaint(toolName string, fn sandbox.InProcessFn, taint types.TaintLevel)
	RegisterRich(toolName string, fn sandbox.InProcessRichFn, taint types.TaintLevel)
	Unregister(toolName string)
}

// MCPManager 管理所有 MCP Server 连接，动态注册工具到 InProcessSandbox。
type MCPManager struct {
	mu               sync.RWMutex
	entries          map[string]*mcpEntry
	sandbox          SandboxToolRegistrar  // 向进程内沙箱注册工具（接口，实现为 *sandbox.InProcessSandbox）
	envelope         *sandbox.ExecEnvelope // 用于 CallTool 下沉
	toolReg          ToolRegistrar         // 可选：MCP 工具同步到 InMemoryToolRegistry，使 Agent FSM 可发现
	catalog          catalog.Catalog       // 新的统一工具目录（仅注册 schema 和 metadata）
	httpClient       *http.Client
	policy           protocol.PolicyGate // 对 process_spawn 的策略检查
	samplingProvider protocol.Provider   // 用于响应 MCP server 的 sampling/createMessage 请求，nil 时禁用 sampling
	onToolsChanged   func()              // 工具集变更时通知调用方（如清除 buildToolSchemas 缓存）
	netApproval      NetApprovalStore    // 网络访问审批持久化；nil 时跳过审批逻辑（安全降级：保持断网）
}

// SetNetApprovalStore 注入网络访问审批存储（SystemRepo）。
// 必须在 LoadFromDB / Add 之前调用；nil 表示跳过审批（默认断网）。
func (m *MCPManager) SetNetApprovalStore(s NetApprovalStore) {
	m.mu.Lock()
	m.netApproval = s
	m.mu.Unlock()
}

// netApprovalKey 生成 preferences 表的 key（mcp.net.approved.<serverID>）。
func netApprovalKey(serverID string) string {
	return "mcp.net.approved." + serverID
}

// checkNetApproval 查询指定 server 的网络访问审批状态。
// 返回 true 表示用户已批准；返回 false 表示 denied 或 pending（safe default）。
func (m *MCPManager) checkNetApproval(ctx context.Context, serverID string) bool {
	m.mu.RLock()
	store := m.netApproval
	m.mu.RUnlock()
	if store == nil {
		return false // 无存储则保守断网
	}
	val, err := store.GetPreference(ctx, netApprovalKey(serverID))
	if err != nil {
		slog.Warn("mcp: failed to read network approval state, defaulting to isolated",
			"server_id", serverID, "err", err)
		return false
	}
	return val == "approved"
}

// SetSamplingProvider 注入 LLM Provider，供 MCP sampling 回调使用。
func (m *MCPManager) SetSamplingProvider(p protocol.Provider) {
	m.mu.Lock()
	m.samplingProvider = p
	m.mu.Unlock()
}

// IsPluginConnected 判断给定 plugin_id 是否有至少一个已连接的 MCP Server。
// 用于 buildAmbientSkillsSection 生成目录行的 MCP 状态标注。
func (m *MCPManager) IsPluginConnected(pluginID string) bool {
	if pluginID == "" {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	prefix := "plugin_" + pluginID + "_"
	for id, e := range m.entries {
		if strings.HasPrefix(id, prefix) && e.errMsg == "" {
			return true
		}
	}
	return false
}

func NewMCPManager(sbx SandboxToolRegistrar, httpClient *http.Client, policy protocol.PolicyGate) *MCPManager {
	return &MCPManager{
		entries:    make(map[string]*mcpEntry),
		sandbox:    sbx,
		httpClient: httpClient,
		policy:     policy,
	}
}

// SetEnvelope 注入 Envelope，供 CallTool 直接路径使用。
func (m *MCPManager) SetEnvelope(env *sandbox.ExecEnvelope) {
	m.mu.Lock()
	m.envelope = env
	m.mu.Unlock()
}

// SetOnToolsChanged 注册工具集变更回调，异步连接完成后触发。
func (m *MCPManager) SetOnToolsChanged(fn func()) {
	m.mu.Lock()
	m.onToolsChanged = fn
	m.mu.Unlock()
}

// SetToolRegistrar 注入 InMemoryToolRegistry，使 MCP 工具同步可被 Agent Kernel FSM 发现。
// 必须在任何 Add() 调用之前设置；否则已连接的服务器工具不会同步。
func (m *MCPManager) SetToolRegistrar(reg ToolRegistrar) {
	m.mu.Lock()
	m.toolReg = reg
	m.mu.Unlock()
}

// SetCatalog 注入统一工具目录（替代 SetToolRegistrar 暴露 schema）。
func (m *MCPManager) SetCatalog(c catalog.Catalog) {
	m.mu.Lock()
	m.catalog = c
	m.mu.Unlock()
}

// Add 连接一个 MCP Server，发现工具并注册到 sandbox。
// 连接失败时仍写入 tombstone entry（errMsg 非空），使 ListServers 能向 UI 暴露失败原因。
// name 必须满足 ^[a-zA-Z0-9_-]+$，否则拒绝安装。
//
//nolint:gocyclo // 顺序连接/初始化/注册流程，分支不可合并；拆子函数会破坏 storeFailed 闭包语义。
func (m *MCPManager) Add(ctx context.Context, serverID, name string, cfg MCPClientConfig) error {
	if err := validateLLMNamePart(name); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, fmt.Sprintf("mcp: server name %q invalid (must match ^[a-zA-Z0-9_-]+$)", name), err)
	}

	if m.policy != nil {
		allowed, pErr := m.policy.IsAuthorized(ctx, "mcp_mgr", "process_spawn", name,
			map[string]any{
				"trust_tier":   cfg.TrustTier,
				"transport":    string(cfg.Transport),
				"sandbox_auto": cfg.SandboxPolicy == "" || cfg.SandboxPolicy == "auto",
			})
		if pErr != nil || !allowed {
			reason := "policy denied"
			if pErr != nil {
				reason = pErr.Error()
			}
			return apperr.New(apperr.CodeForbidden, fmt.Sprintf("mcp_manager: process_spawn denied for %q: %s", name, reason))
		}
	}

	// 网络访问审批解析：TrustTier<=2 时 bwrap 默认断网；
	// 若服务器声明 RequiresNetwork=true，则查询 preferences 表决定是否放行。
	// 此处修改的是值拷贝（cfg 按值传入），不影响调用方持有的原始配置。
	if cfg.RequiresNetwork && cfg.TrustTier <= 2 {
		cfg.NetworkApproved = m.checkNetApproval(ctx, serverID)
		if !cfg.NetworkApproved {
			slog.Warn("mcp: server requires network access but approval pending; running with --unshare-net",
				"server_id", serverID, "server", name,
				"hint", "POST /v1/mcp-servers/{id}/network-access to approve")
		}
	}

	slog.Info("mcp_manager: starting mcp server", "id", serverID, "name", name, "transport", cfg.Transport, "command", cfg.Command)

	m.mu.Lock()

	if old, ok := m.entries[serverID]; ok {
		if old.client != nil {
			old.client.Close()
		}
		m.unregisterTools(old.name, old.tools)
	}

	storeFailed := func(err error) error {
		m.entries[serverID] = &mcpEntry{name: name, cfg: cfg, errMsg: err.Error()}
		m.mu.Unlock()
		slog.Error("mcp_manager: start server failed", "id", serverID, "name", name, "err", err)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "MCPManager.Add", err)
		}
		return nil
	}

	client := NewMCPClient(cfg, m.httpClient)

	if err := client.Connect(ctx); err != nil {
		wrapped := apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp_manager: connect %q", serverID), err)
		return storeFailed(wrapped)
	}
	if m.samplingProvider != nil {
		client.SetServerRequestHandler(m.makeSamplingHandler())
	}
	if err := client.Initialize(ctx); err != nil {
		client.Close()
		wrapped := apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp_manager: initialize %q", serverID), err)
		return storeFailed(wrapped)
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		client.Close()
		wrapped := apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mcp_manager: list tools %q", serverID), err)
		return storeFailed(wrapped)
	}

	validTools := m.registerTools(name, client, tools)
	m.entries[serverID] = &mcpEntry{
		client: client,
		name:   name,
		cfg:    cfg,
		tools:  validTools,
	}
	notify := m.onToolsChanged
	m.mu.Unlock()

	slog.Info("mcp_manager: server connected", "id", serverID, "tools", len(tools))
	// 锁外触发回调，避免回调内反向加锁导致死锁
	if notify != nil {
		notify()
	}
	return nil
}

// Remove 断开并移除 MCP Server，取消注册其所有工具。
func (m *MCPManager) Remove(serverID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.entries[serverID]; ok {
		if e.client != nil {
			e.client.Close()
		}
		m.unregisterTools(e.name, e.tools)
		delete(m.entries, serverID)
	}
}

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

// ListServers 返回所有 MCP Server 的运行时状态快照。
func (m *MCPManager) ListServers() []MCPServerInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]MCPServerInfo, 0, len(m.entries))
	for id, e := range m.entries {
		result = append(result, MCPServerInfo{
			ID:        id,
			Name:      e.name,
			Transport: string(e.cfg.Transport),
			Connected: e.errMsg == "",
			Tools:     e.tools,
			Error:     e.errMsg,
		})
	}
	return result
}

// ListToolSchemas 返回所有已连接 MCP 工具的 ToolSchema，用于注入 InferRequest。
func (m *MCPManager) ListToolSchemas() []types.ToolSchema {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []types.ToolSchema
	for _, e := range m.entries {
		for _, t := range e.tools {
			var schema any
			json.Unmarshal(t.InputSchema, &schema) //nolint:errcheck
			result = append(result, types.ToolSchema{
				Name:        MCPToolName(e.name, t.Name),
				Description: t.Description,
				Parameters:  schema,
			})
		}
	}
	return result
}

// LoadFromDB 启动时从数据库加载并异步连接所有已启用的 MCP Server。
// 统一加载独立安装的 MCP（plugin_id=”）和插件内嵌的 MCP（plugin_id != ”）。
// dataDir 用于展开 args/url 中的 {DATA_DIR} 占位符；plugin MCP 的工作目录由 work_dir 字段提供。
func (m *MCPManager) LoadFromDB(ctx context.Context, extRepo protocol.ExtensionRepository, dataDir string) {
	servers, err := extRepo.ListMCPServers(ctx)
	if err != nil {
		slog.Error("mcp_manager: load from db", "err", err)
		return
	}

	for _, s := range servers {
		if !s.Enabled {
			continue
		}
		var args []string
		json.Unmarshal([]byte(s.Args), &args) //nolint:errcheck
		var env map[string]string
		json.Unmarshal([]byte(s.Env), &env) //nolint:errcheck

		for i, a := range args {
			args[i] = strings.ReplaceAll(a, "{DATA_DIR}", dataDir)
		}

		resolvedURL := strings.ReplaceAll(s.URL, "{DATA_DIR}", dataDir)
		// "streamable_http" 是数据库存储值，兼容 Claude Code 的 "streamable-http" 别名
		transport := s.Transport
		if transport == "streamable-http" {
			transport = string(MCPStreamableHTTP)
		}
		cfg := MCPClientConfig{
			Transport:  MCPTransport(transport),
			Command:    s.Command,
			Args:       args,
			Env:        env,
			URL:        resolvedURL,
			WorkDir:    s.WorkDir, // plugin MCP 设为 install_path；独立 MCP 为空（继承父进程 cwd）
			Timeout:    time.Duration(s.Timeout) * time.Second,
			ServerName: s.Name,
			// TrustTier 驱动沙箱策略（bwrap 网络隔离）和污点等级（TaintMedium/TaintHigh）。
			// SandboxPolicy 不设置（""）：applyStdioSandbox 将其视为 "auto"，自动按 TrustTier 决策。
			TrustTier:       s.TrustTier,
			Trusted:         s.TrustTier >= 3,
			RequiresNetwork: s.RequiresNetwork,
			// NetworkApproved 由 Add() 内部按 preferences 表动态解析，此处留零值。
		}
		// 每个 server 独立 goroutine，避免一个慢连接阻塞其他
		concurrent.SafeGo(ctx, "mcp_manager_add", func(_ context.Context) {
			connCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			if err := m.Add(connCtx, s.ID, s.Name, cfg); err != nil {
				slog.Error("mcp_manager: load server failed", "id", s.ID, "err", err)
			}
		})
	}
}

// maxLLMToolNameLen 是 OpenAI 兼容接口对 function.name 的最大长度限制。
const maxLLMToolNameLen = 64

// registerTools 注册合法的 MCP 工具到 sandbox，返回实际注册成功的工具子集。
// 服务器名（serverName）在 Add() 中已经过 validateLLMNamePart 校验，此处信任。
// 工具名来自外部 MCP 服务器，不可控：非法字符静默替换并记录警告；超长则跳过。
func (m *MCPManager) registerTools(serverName string, client *MCPClient, tools []MCPTool) []MCPTool {
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
		fn := makeMCPToolFn(client, t.Name)
		// RegisterRich 将 MCP 工具注册到富工具路径（支持 ImageParts 回传）
		m.sandbox.RegisterRich(llmName, fn, taint)
		// 同步到 InMemoryToolRegistry (逐步废弃)
		if m.toolReg != nil {
			riskLevel := types.RiskHigh
			if client.cfg.Trusted {
				riskLevel = types.RiskMedium
			}
			regErr := m.toolReg.Register(types.Tool{
				Name:        llmName,
				Description: t.Description,
				InputSchema: t.InputSchema,
				Source:      types.ToolMCP,
				RiskLevel:   riskLevel,
				TrustTier:   types.TrustTier(client.cfg.TrustTier),
				Timeout:     toolTimeout,
			})
			if regErr != nil {
				slog.Warn("mcp: failed to sync tool to InMemoryToolRegistry", "server", serverName, "tool", llmName, "err", regErr)
			}
		}

		// 注册到统一工具目录 Catalog
		if m.catalog != nil {
			m.catalog.Register(catalog.CatalogEntry{
				Name:        llmName,
				Description: t.Description,
				Parameters:  t.InputSchema,
				Source:      types.ToolMCP,
				TrustTier:   types.TrustTier(client.cfg.TrustTier),
				TaintLevel:  taint,
				Timeout:     toolTimeout,
				MCPServerID: client.cfg.ServerName,
				MCPToolName: t.Name,
			})
		}

		valid = append(valid, t)
	}
	return valid
}

// makeMCPToolFn 创建调用 MCP 工具的富执行函数。
// 返回完整 ToolResult（含 ImageParts），使用 CallToolTainted 进行污点保护反序列化（M07 §1 安全要求）。
func makeMCPToolFn(client *MCPClient, mcpName string) sandbox.InProcessRichFn {
	return func(ctx context.Context, spec sandbox.SandboxSpec) (*types.ToolResult, error) {
		var args map[string]any
		if len(spec.Input) > 0 {
			json.Unmarshal(spec.Input, &args) //nolint:errcheck
		}
		// CallToolTainted 内部执行 TaintPreservingDecoder，taint 通过 RegisterRich 传递
		text, imgs, _, err := client.CallToolTainted(ctx, mcpName, args)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeMCPToolFn", err)
		}
		return &types.ToolResult{
			Success:    true,
			Output:     []byte(text),
			ImageParts: imgs, // MCP type="image" content block 解析结果
		}, nil
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
}

// DynamicConnect 动态连接一个 MCP server 并注册其工具到沙箱。
// 幂等：同名 server 已连接时直接返回 nil。
func (m *MCPManager) DynamicConnect(ctx context.Context, req DynamicConnectRequest) error {
	// ServerName 用作工具名前缀（mcp__<ServerName>__<tool>），必须满足 ^[a-zA-Z0-9_-]+$。
	// Add() 已内置此校验；DynamicConnect 走独立路径，需显式检查。
	if err := validateLLMNamePart(req.ServerName); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput,
			fmt.Sprintf("mcp: DynamicConnect server name %q invalid (must match ^[a-zA-Z0-9_-]+$)", req.ServerName), err)
	}
	m.mu.Lock()
	if _, exists := m.entries[req.ServerName]; exists {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	cfg := MCPClientConfig{
		ServerName: req.ServerName,
		Transport:  MCPTransport(req.Transport),
		Command:    req.Command,
		Args:       req.Args,
		URL:        req.URL,
	}
	client := NewMCPClient(cfg, m.httpClient)
	if err := client.Connect(ctx); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "MCPManager.DynamicConnect", err)
	}
	if err := client.Initialize(ctx); err != nil {
		client.Close()
		return apperr.Wrap(apperr.CodeInternal, "MCPManager.DynamicConnect", err)
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		client.Close()
		return apperr.Wrap(apperr.CodeInternal, "MCPManager.DynamicConnect", err)
	}

	validTools := m.registerTools(req.ServerName, client, tools)

	m.mu.Lock()
	if _, exists := m.entries[req.ServerName]; exists {
		m.mu.Unlock()
		client.Close()
		return nil
	}
	m.entries[req.ServerName] = &mcpEntry{
		client: client,
		name:   req.ServerName,
		cfg:    cfg,
		tools:  validTools,
	}
	notify := m.onToolsChanged
	m.mu.Unlock()

	if notify != nil {
		notify()
	}
	return nil
}

// ApproveNetworkAccess 设置服务器的网络访问审批状态并立即重启该 MCP 连接，
// 使新的网络隔离策略立即生效（approved=true → 放行网络；false → 恢复断网）。
//
// 此方法：
//  1. 将 "approved"/"denied" 写入 preferences 表（持久化）。
//  2. 从 DB 读取最新配置（含 RequiresNetwork 字段）。
//  3. Remove + Add 重启连接（与 Update() 的重连模式相同）。
func (m *MCPManager) ApproveNetworkAccess(ctx context.Context, serverID string, extRepo protocol.ExtensionRepository, dataDir string, approved bool) error {
	m.mu.RLock()
	store := m.netApproval
	m.mu.RUnlock()
	if store == nil {
		return apperr.New(apperr.CodeInternal, "mcp: net approval store not configured")
	}

	// 1. 持久化审批结果
	decision := "denied"
	if approved {
		decision = "approved"
	}
	if err := store.UpsertPreference(ctx, netApprovalKey(serverID), decision); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mcp: persist net approval", err)
	}
	slog.Info("mcp: network access decision recorded",
		"server_id", serverID, "decision", decision)

	// 2. 读取 DB 当前配置（重连时需要完整 row）
	row, err := extRepo.GetMCPServer(ctx, serverID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mcp: get server for reconnect", err)
	}
	if row == nil {
		return apperr.New(apperr.CodeNotFound, "mcp: server not found: "+serverID)
	}
	if !row.Enabled {
		// 服务器当前禁用，仅存储决策，不触发重连
		return nil
	}

	// 3. 异步重启连接（与 Update 模式一致，不阻塞当前请求）
	var args []string
	var env map[string]string
	if err := json.Unmarshal([]byte(row.Args), &args); err != nil {
		args = nil
	}
	if err := json.Unmarshal([]byte(row.Env), &env); err != nil {
		env = nil
	}
	for i, a := range args {
		args[i] = strings.ReplaceAll(a, "{DATA_DIR}", dataDir)
	}
	transport := row.Transport
	if transport == "streamable-http" {
		transport = string(MCPStreamableHTTP)
	}
	clientCfg := MCPClientConfig{
		Transport:       MCPTransport(transport),
		Command:         row.Command,
		Args:            args,
		Env:             env,
		URL:             strings.ReplaceAll(row.URL, "{DATA_DIR}", dataDir),
		WorkDir:         row.WorkDir,
		Timeout:         time.Duration(row.Timeout) * time.Second,
		ServerName:      row.Name,
		TrustTier:       row.TrustTier,
		Trusted:         row.TrustTier >= 3,
		RequiresNetwork: row.RequiresNetwork,
		// NetworkApproved 由 Add() 内部重新查询 preferences，此处不预设
	}
	m.Remove(serverID)
	concurrent.SafeGo(context.Background(), "mcp_net_approve_reconnect", func(_ context.Context) {
		bgCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := m.Add(bgCtx, serverID, row.Name, clientCfg); err != nil {
			slog.Warn("mcp: reconnect after network approval failed", "server_id", serverID, "err", err)
		}
	})
	return nil
}

// MCPUpdateConfig protocol.MCPUpdateConfig 本地别名，使包内调用无需显式引用 protocol 包。
type MCPUpdateConfig = protocol.MCPUpdateConfig

// Update 更新并重启 MCP 连接，同时同步 DB。
func (m *MCPManager) Update(ctx context.Context, extRepo protocol.ExtensionRepository, id string, cfg MCPUpdateConfig, dataDir string) error {
	argsBytes, _ := json.Marshal(cfg.Args)
	envBytes, _ := json.Marshal(cfg.Env)

	requiresNetworkInt := 0
	if cfg.RequiresNetwork {
		requiresNetworkInt = 1
	}
	fields := map[string]any{
		"name":             cfg.Name,
		"transport":        cfg.Transport,
		"command":          cfg.Command,
		"args":             string(argsBytes),
		"env":              string(envBytes),
		"url":              cfg.URL,
		"enabled":          cfg.Enabled,
		"timeout":          cfg.Timeout,
		"trust_tier":       cfg.TrustTier,
		"requires_network": requiresNetworkInt,
	}

	if err := extRepo.UpdateMCPServer(ctx, id, fields); err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return apperr.New(apperr.CodeNotFound, "mcp server not found")
		}
		return apperr.Wrap(apperr.CodeInternal, "MCPManager.Update", err)
	}

	m.Remove(id)
	if cfg.Enabled {
		clientCfg := MCPClientConfig{
			Transport:  MCPTransport(cfg.Transport),
			Command:    cfg.Command,
			Args:       make([]string, len(cfg.Args)),
			Env:        cfg.Env,
			URL:        strings.ReplaceAll(cfg.URL, "{DATA_DIR}", dataDir),
			Timeout:    time.Duration(cfg.Timeout) * time.Second,
			ServerName: cfg.Name,
			// TrustTier 驱动沙箱策略和污点等级；SandboxPolicy 不设置（""=auto）。
			TrustTier:       cfg.TrustTier,
			Trusted:         cfg.TrustTier >= 3,
			RequiresNetwork: cfg.RequiresNetwork,
			// NetworkApproved 由 Add() 内部按 preferences 表动态解析，此处留零值。
		}
		for i, a := range cfg.Args {
			clientCfg.Args[i] = strings.ReplaceAll(a, "{DATA_DIR}", dataDir)
		}
		concurrent.SafeGo(context.Background(), "mcp_manager_update", func(_ context.Context) {
			bgCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if err := m.Add(bgCtx, id, cfg.Name, clientCfg); err != nil {
				slog.Warn("mcp: connect server failed after update", "id", id, "err", err)
			}
		})
	}
	return nil
}

// makeSamplingHandler 构建 MCP server 主动请求处理器，支持 sampling/createMessage 和 roots/list。
func (m *MCPManager) makeSamplingHandler() ServerRequestHandler {
	return func(ctx context.Context, method string, id int64, params json.RawMessage) (json.RawMessage, error) {
		switch method {
		case "sampling/createMessage":
			if m.samplingProvider == nil {
				return nil, apperr.New(apperr.CodeInternal, "sampling: no provider configured")
			}
			var req struct {
				Messages  []types.Message `json:"messages"`
				MaxTokens int             `json:"maxTokens"`
			}
			if err := json.Unmarshal(params, &req); err != nil {
				return nil, apperr.Wrap(apperr.CodeInvalidInput, "sampling: invalid params", err)
			}
			opts := []types.InferOption{}
			if req.MaxTokens > 0 {
				opts = append(opts, types.WithMaxTokens(req.MaxTokens))
			}
			resp, err := m.samplingProvider.Infer(ctx, req.Messages, opts...)
			if err != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "MCPManager.makeSamplingHandler", err)
			}
			result, _ := json.Marshal(map[string]any{
				"role":    "assistant",
				"content": map[string]any{"type": "text", "text": resp.Content},
				"model":   resp.Model,
			})
			return result, nil
		case "roots/list":
			// 返回空 roots 列表（当前不暴露文件系统 roots）
			result, _ := json.Marshal(map[string]any{"roots": []any{}})
			return result, nil
		default:
			return nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("mcp: unsupported server method %q", method))
		}
	}
}
