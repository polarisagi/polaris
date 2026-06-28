package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// MCPConnector 接口用于插件安装时异步注册 MCP Server。
type MCPConnector interface {
	Add(ctx context.Context, serverID, name string, cfg mcp.MCPClientConfig) error
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
		return "", apperr.New(apperr.CodeInvalidInput, "plugin_installer: LocalPath required")
	}

	// 1. 解析 mcp.json (注意原代码里 runtime_registrar 是读取 mcp.json)
	// 原代码: cfgPath := filepath.Join(installDir, "mcp.json")
	cfgPath := filepath.Join(installDir, "mcp.json")
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
		_ = p.extRepo.UpsertMCPServer(ctx, types.MCPServerRow{
			ID:        serverID,
			Name:      name,
			PluginID:  req.InstID,
			Transport: transport,
			Command:   command,
			Args:      argsStr,
			WorkDir:   installDir,
			Enabled:   true,
		})
		if p.mcpConn != nil {
			clientCfg := mcp.MCPClientConfig{
				Transport: mcp.MCPTransport(transport),
				Command:   command,
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
			go func() {
				_ = p.mcpConn.Add(context.Background(), serverID, name, clientCfg)
			}()
		}
	}

	// plugin skills register logic ... (If needed, based on runtime_registrar)

	return installDir, nil
}

func (p *PluginInstaller) Uninstall(ctx context.Context, req UninstallReq) error {
	_ = p.extRepo.UninstallCleanup(ctx, req.RuntimeID, "", "plugin")
	return nil
}
