package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// PluginVarsResolver 解析插件组件运行期变量（插件根、数据目录、userConfig）。
// consumer-side 定义；实现在 lifecycle（读 plugins 表与用户配置存储）。
type PluginVarsResolver interface {
	ResolvePluginVars(ctx context.Context, pluginID string) (pluginspec.Vars, error)
}

// SetRowSource 注入 mcp_servers 读取源与数据根目录。StartFromDB 与 RestoreServersFromDB
// 共用同一转换逻辑——此前五个调用点各自拼装配置，headers / 插件变量均未覆盖。
func (m *MCPManager) SetRowSource(repo protocol.ExtensionRepository, dataDir string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rowRepo = repo
	m.dataDir = dataDir
}

// SetPluginVarsResolver 注入插件变量解析器；未注入时插件 MCP 中的插件变量无法展开，
// ConfigFromRow 对插件行返回错误而不是带着字面 "${...}" 启动进程。
func (m *MCPManager) SetPluginVarsResolver(r PluginVarsResolver) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.varsResolver = r
}

// StartFromDB 按 mcp_servers 行（权威源）启动或重启连接；行禁用时仅断开。
func (m *MCPManager) StartFromDB(ctx context.Context, serverID string) error {
	m.mu.RLock()
	repo := m.rowRepo
	m.mu.RUnlock()
	if repo == nil {
		return apperr.New(apperr.CodeInternal, "mcp_manager: row source not configured")
	}
	row, err := repo.GetMCPServer(ctx, serverID)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mcp_manager.StartFromDB", err)
	}
	if row == nil {
		return apperr.New(apperr.CodeNotFound, "mcp_manager: server not found: "+serverID)
	}
	m.Remove(serverID)
	if !row.Enabled {
		return nil
	}
	cfg, err := m.ConfigFromRow(ctx, *row)
	if err != nil {
		return err
	}
	return m.Add(ctx, row.ID, row.Name, cfg)
}

// ConfigFromRow 行 → 客户端配置：{DATA_DIR} 旧占位、插件变量（ADR-0103 决策二）、headers。
func (m *MCPManager) ConfigFromRow(ctx context.Context, row types.MCPServerRow) (MCPClientConfig, error) {
	var args []string
	var env, headers map[string]string
	// L2：行内 JSON 损坏时拒绝启动——带着缺失的参数/凭据头启动进程只会得到难以排查的连接失败。
	for field, pair := range map[string]struct {
		raw string
		dst any
	}{"args": {row.Args, &args}, "env": {row.Env, &env}, "headers": {row.Headers, &headers}} {
		if pair.raw == "" {
			continue
		}
		if err := json.Unmarshal([]byte(pair.raw), pair.dst); err != nil {
			return MCPClientConfig{}, apperr.Wrap(apperr.CodeInvalidInput, "mcp_manager.ConfigFromRow: "+field, err)
		}
	}

	m.mu.RLock()
	dataDir, resolver := m.dataDir, m.varsResolver
	m.mu.RUnlock()
	cfg := MCPClientConfig{
		Transport:       normalizeRowTransport(row.Transport),
		Command:         row.Command,
		URL:             strings.ReplaceAll(row.URL, "{DATA_DIR}", dataDir),
		WorkDir:         row.WorkDir,
		Timeout:         time.Duration(row.Timeout) * time.Second,
		ServerName:      row.Name,
		TrustTier:       row.TrustTier,
		Trusted:         row.TrustTier >= 3,
		RequiresNetwork: row.RequiresNetwork,
	}
	for i, a := range args {
		args[i] = strings.ReplaceAll(a, "{DATA_DIR}", dataDir)
	}
	cfg.Args, cfg.Env, cfg.Headers = args, env, headers
	if row.PluginID == "" {
		return cfg, nil
	}
	if resolver == nil {
		return cfg, apperr.New(apperr.CodeInternal, "mcp_manager: plugin vars resolver not configured")
	}
	vars, err := resolver.ResolvePluginVars(ctx, row.PluginID)
	if err != nil {
		return cfg, apperr.Wrap(apperr.CodeOf(err), "mcp_manager.ConfigFromRow", err)
	}
	vars.HostEnv = allowedHostEnv()
	return expandPluginConfig(cfg, vars), nil
}

func expandPluginConfig(cfg MCPClientConfig, vars pluginspec.Vars) MCPClientConfig {
	var unresolved []string
	expand := func(s string) string {
		out, u := vars.Expand(s)
		unresolved = append(unresolved, u...)
		return out
	}
	cfg.Command = expand(cfg.Command)
	cfg.URL = expand(cfg.URL)
	cfg.WorkDir = expand(cfg.WorkDir)
	var u []string
	cfg.Args, cfg.Env, u = vars.ExpandAll(cfg.Args, cfg.Env)
	unresolved = append(unresolved, u...)
	_, cfg.Headers, u = vars.ExpandAll(nil, cfg.Headers)
	unresolved = append(unresolved, u...)
	if cfg.Transport == MCPStdio {
		if cfg.Env == nil {
			cfg.Env = map[string]string{}
		}
		for k, v := range vars.ProcessEnv() {
			cfg.Env[k] = v
		}
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = vars.PluginRoot // agent-plugins：stdio cwd 缺省为插件根
	}
	if len(unresolved) > 0 {
		slog.Warn("mcp_manager: unresolved plugin variables expanded to empty", "server", cfg.ServerName, "vars", unresolved)
	}
	return cfg
}

// normalizeRowTransport 行内 transport 统一为客户端枚举（兼容 Claude 的 "streamable-http" / "http"）。
func normalizeRowTransport(t string) MCPTransport {
	switch t {
	case "http", "streamable-http", "streamable_http":
		return MCPStreamableHTTP
	case "":
		return MCPStdio
	}
	return MCPTransport(t)
}

// allowedHostEnv 插件变量可展开的宿主环境变量 = MCP 子进程白名单（R1.15）。
func allowedHostEnv() map[string]string {
	out := map[string]string{}
	for _, kv := range sanitizeParentEnv() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}
