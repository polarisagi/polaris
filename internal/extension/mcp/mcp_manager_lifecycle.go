package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// 只读查询 (ListServers/ListToolSchemas)、DB 加载 (RestoreServersFromDB)、
// 动态连接 (DynamicConnect)、配置更新 (Update) （R7 拆分自 mcp_manager.go）。
// 核心连接管理 (Add/Remove/GetClient) 见 mcp_manager.go。
// ============================================================================

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

// RestoreServersFromDB 启动时从数据库加载并异步连接所有已启用的 MCP Server。
// 统一加载独立安装的 MCP（plugin_id=”）和插件内嵌的 MCP（plugin_id != ”）。
// dataDir 用于展开 args/url 中的 {DATA_DIR} 占位符；plugin MCP 的工作目录由 work_dir 字段提供。
func (m *MCPManager) RestoreServersFromDB(ctx context.Context, extRepo protocol.ExtensionRepository, dataDir string) {
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

// DynamicConnect 以运行时动态方式连接一个尚未持久化的 MCP Server（不经过 Add() 的 DB 写入路径），
// 用于临时/一次性连接场景：校验 ServerName 合法性 → 建立连接 → 拉取并注册工具 → 登记到内存 entries。
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

// Update 更新已持久化的 MCP Server 配置：写库 → Remove 断开旧连接 → 若 Enabled 则异步 Add 重新建立连接。
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
