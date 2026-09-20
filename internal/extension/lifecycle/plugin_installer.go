package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// MCPConnector 接口用于插件安装时异步注册 MCP Server。
type MCPConnector interface {
	Add(ctx context.Context, serverID, name string, cfg mcp.MCPClientConfig) error
	GetClient(serverID string) protocol.MCPClient // returns protocol.MCPClient avoiding circular deps
}

// PluginInstaller 处理 plugin 类型：
// 1. 读取 install_path/.mcp.json 解析 MCP server 配置
// 2. 写 mcp_servers 表
// 3. 调用 MCPConnector.Add 异步连接
// 4. 读取 install_path/skills/ 目录，注册 skills 表
// 5. UpdateInstanceStatus("installed") (由 InstallFSM 统一处理)
type PluginInstaller struct {
	extRepo  protocol.ExtensionRepository
	mcpConn  MCPConnector
	skillReg protocol.SkillRegistry
	// policyGate 对插件内嵌子 MCP 逐个独立授权（extension/CLAUDE.md 硬约束 3，
	// GR-8-002）。父插件的安装授权不能代替子 MCP：子 MCP 会拉起任意本地进程
	// 并对 Agent 暴露工具，风险面与插件本身不同。nil 时 fail-closed 跳过全部子 MCP。
	policyGate protocol.PolicyGate
}

// WithPolicyGate 注入子 MCP 授权网关。
func (p *PluginInstaller) WithPolicyGate(pg protocol.PolicyGate) *PluginInstaller {
	p.policyGate = pg
	return p
}

// authorizeBundleMCP 对单个内嵌子 MCP 调用 PolicyGate；拒绝或出错返回 false
// （调用方 skip+Warn，不中断父插件安装）。
func (p *PluginInstaller) authorizeBundleMCP(ctx context.Context, req InstallReq, serverID, command string) bool {
	if p.policyGate == nil {
		slog.Warn("plugin_installer: no policy gate, bundle mcp skipped (fail-closed)", "plugin", req.InstID, "server", serverID)
		return false
	}
	res, err := p.policyGate.Review(ctx, types.PolicyReviewRequest{
		Principal: "plugin:" + req.InstID,
		Action:    "install_extension",
		Resource:  serverID,
		Context: map[string]any{
			"trust_level":   req.TrustTier,
			"publisher":     req.Publisher,
			"ext_type":      string(types.TypeMCP),
			"bundle_parent": req.InstID,
			"command":       command,
		},
	})
	if err != nil || !res.Allowed {
		slog.Warn("plugin_installer: bundle mcp denied by policy, skipped",
			"plugin", req.InstID, "server", serverID, "reason", res.Reason, "err", err)
		return false
	}
	return true
}

func NewPluginInstaller(extRepo protocol.ExtensionRepository, mcpConn MCPConnector, skillReg protocol.SkillRegistry) *PluginInstaller {
	return &PluginInstaller{
		extRepo:  extRepo,
		mcpConn:  mcpConn,
		skillReg: skillReg,
	}
}

func (p *PluginInstaller) ExtType() types.ExtType { return types.TypePlugin }

func (p *PluginInstaller) Install(ctx context.Context, req InstallReq) (string, error) {
	installDir := req.LocalPath
	if installDir == "" {
		return "", nil // LocalPath 为空，代表可能只是占位注册（例如通过 catalog 异步安装），不实际处理
	}

	// 1. 解析 mcp.json 或 .mcp.json
	cfgPath, err := protocol.FindMCPConfig(installDir)
	if err != nil {
		return installDir, nil //nolint:nilerr // 没有配置，跳过 MCP 注册
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return installDir, nil //nolint:nilerr
	}

	var mcpCfgs []map[string]any
	// 检查是数组还是单个对象
	var single map[string]any
	if err := json.Unmarshal(raw, &mcpCfgs); err != nil {
		if err := json.Unmarshal(raw, &single); err == nil {
			mcpCfgs = append(mcpCfgs, single)
		}
	}

	// 2 & 3. 注册 mcp_servers
	for i, cfg := range mcpCfgs {
		p.registerBundleMCP(ctx, req, installDir, i, cfg)
	}

	// plugin skills register logic ... (If needed, based on runtime_registrar)

	return installDir, nil
}

func (p *PluginInstaller) Uninstall(ctx context.Context, req UninstallReq) error {
	_ = p.extRepo.UninstallCleanup(ctx, req.RuntimeID, "", "plugin")
	return nil
}

// registerBundleMCP 注册插件内嵌的单个子 MCP（先独立授权，拒绝则 skip）。
// 从 Install 拆出以控制圈复杂度。
func (p *PluginInstaller) registerBundleMCP(ctx context.Context, req InstallReq, installDir string, i int, cfg map[string]any) {
	name, _ := cfg["name"].(string)
	if name == "" {
		name = fmt.Sprintf("plugin-mcp-%d", i)
	}
	transport, _ := cfg["transport"].(string)
	command, _ := cfg["command"].(string)

	var argsStr string
	if args, ok := cfg["args"].([]any); ok {
		strArgs := make([]string, len(args))
		for j, a := range args {
			strArgs[j] = fmt.Sprint(a)
		}
		b, _ := json.Marshal(strArgs)
		argsStr = string(b)
	}

	serverID := "plugin_" + req.InstID + "_" + name
	if !p.authorizeBundleMCP(ctx, req, serverID, command) {
		return
	}
	if err := p.extRepo.UpsertMCPServer(ctx, types.MCPServerRow{
		ID:        serverID,
		Name:      name,
		PluginID:  req.InstID,
		Transport: transport,
		Command:   command,
		Args:      argsStr,
		WorkDir:   installDir,
		Enabled:   true,
	}); err != nil {
		// L2：子 MCP 持久化失败不中断父插件安装（硬约束 3），但不能再启动一个
		// 重启后无法恢复的进程；留痕并跳过。
		slog.Warn("plugin_installer: persist bundle mcp failed, skipped", "plugin", req.InstID, "server", serverID, "err", err)
		return
	}
	if p.mcpConn != nil {
		clientCfg := mcp.MCPClientConfig{
			Transport: mcp.MCPTransport(transport),
			Command:   command,
			TrustTier: req.TrustTier,
		}
		if args, ok := cfg["args"].([]any); ok {
			clientCfg.Args = make([]string, len(args))
			for j, a := range args {
				clientCfg.Args[j] = fmt.Sprint(a)
			}
		}
		if env, ok := cfg["env"].(map[string]any); ok {
			clientCfg.Env = make(map[string]string)
			for k, v := range env {
				clientCfg.Env[k] = fmt.Sprint(v)
			}
		}
		concurrent.SafeGo(context.Background(), "plugin_installer.mcp_add", func(sgCtx context.Context) {
			if err := p.mcpConn.Add(sgCtx, serverID, name, clientCfg); err != nil {
				slog.Warn("plugin_installer: start bundle mcp failed", "plugin", req.InstID, "server", serverID, "err", err)
			}
		})
	}
}
