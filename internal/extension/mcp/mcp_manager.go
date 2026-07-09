package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/apperr"
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

func (m *MCPManager) GetClient(serverID string) interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if e, ok := m.entries[serverID]; ok {
		return e.client
	}
	return nil
}

// 只读查询 (ListServers/ListToolSchemas)、DB 加载 (RestoreServersFromDB)、动态
// 连接 (DynamicConnect)、配置更新 (Update) 见 mcp_manager_lifecycle.go（R7 拆分）。
