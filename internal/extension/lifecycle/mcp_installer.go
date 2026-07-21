package lifecycle

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/knowledge/connector"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type MCPInstaller struct {
	extRepo  protocol.ExtensionRepository
	mcpConn  MCPConnector
	registry *connector.Registry // 可选注入（2026-07-04 修复：不再用包级 GlobalRegistry）
}

func NewMCPInstaller(extRepo protocol.ExtensionRepository, mcpConn MCPConnector) *MCPInstaller {
	return &MCPInstaller{
		extRepo: extRepo,
		mcpConn: mcpConn,
	}
}

// WithRegistry 注入知识源连接器注册表（可选；未注入时 knowledge-source
// 能力声明会被静默忽略，不影响其余 MCP 安装流程）。
func (m *MCPInstaller) WithRegistry(r *connector.Registry) *MCPInstaller {
	m.registry = r
	return m
}

func (m *MCPInstaller) ExtType() types.ExtType { return types.TypeMCP }

func (m *MCPInstaller) Install(ctx context.Context, req InstallReq) (string, error) {
	installDir := req.LocalPath
	if installDir == "" {
		return "", apperr.New(apperr.CodeInvalidInput, "mcp_installer: LocalPath required")
	}

	if m.mcpConn == nil {
		return installDir, nil
	}

	cfgPath, err := protocol.FindMCPConfig(installDir)
	if err != nil {
		slog.Warn("mcp_installer: mcp.json not found, skip runtime registration",
			"inst_id", req.InstID, "dir", installDir)
		return installDir, nil //nolint:nilerr
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return installDir, apperr.Wrap(apperr.CodeInternal, "mcp_installer: read mcp.json", err)
	}

	var mcpCfg struct {
		Name         string            `json:"name"`
		Transport    string            `json:"transport"`
		Command      string            `json:"command"`
		Args         []string          `json:"args"`
		Endpoint     string            `json:"endpoint"`
		Env          map[string]string `json:"env"`
		Capabilities []string          `json:"capabilities"`
	}
	if jsonErr := json.Unmarshal(raw, &mcpCfg); jsonErr != nil {
		return installDir, apperr.Wrap(apperr.CodeInvalidInput, "mcp_installer: parse mcp.json", jsonErr)
	}

	name := mcpCfg.Name
	if name == "" {
		name = strings.TrimPrefix(req.InstID, "ext_")
	}

	clientCfg := mcp.MCPClientConfig{
		Transport: mcp.MCPTransport(mcpCfg.Transport),
		Command:   mcpCfg.Command,
		Args:      mcpCfg.Args,
		URL:       mcpCfg.Endpoint,
		Env:       mcpCfg.Env,
	}

	if addErr := m.mcpConn.Add(ctx, req.InstID, name, clientCfg); addErr != nil {
		return installDir, apperr.Wrap(apperr.CodeInternal, "mcp_installer", addErr)
	}

	// 检查是否声明了知识源能力（2026-07-21 deadcode 审查补齐：
	// connector.MCPKnowledgeConnector.List/Fetch 此前是 CodeUnimplemented 桩实现，
	// 曾在此处硬拦截避免 SyncScheduler 对空转桩代码指数退避重试；现已接入真实
	// mcp.MCPClient.ResourcesList/ResourcesRead RPC 调用，可以正常注册）。
	if m.registry != nil {
		for _, cap := range mcpCfg.Capabilities {
			if cap != "knowledge-source" {
				continue
			}
			rawClient := m.mcpConn.GetClient(req.InstID)
			knowledgeClient, ok := rawClient.(connector.MCPClient)
			if !ok {
				slog.Warn("mcp_installer: knowledge-source capability declared but client does not support resources/list+resources/read, skipping registration",
					"inst_id", req.InstID, "name", name)
				continue
			}
			m.registry.Register(connector.NewMCPKnowledgeConnector(req.InstID, name, knowledgeClient))
			slog.Info("mcp_installer: knowledge-source MCP server registered with SyncScheduler",
				"inst_id", req.InstID, "name", name)
		}
	}

	slog.Info("mcp_installer: MCP server registered",
		"inst_id", req.InstID, "name", name, "transport", mcpCfg.Transport)
	return installDir, nil
}

func (m *MCPInstaller) Uninstall(ctx context.Context, req UninstallReq) error {
	// 与上面新增的知识源连接器注册对称：卸载时一并摘除，避免 SyncScheduler
	// 继续调度一个客户端已被拆除的 connector（2026-07-21 随 List/Fetch 接入一并修复）。
	if m.registry != nil {
		m.registry.Unregister(req.InstID)
	}
	_ = m.extRepo.UninstallCleanup(ctx, "", req.RuntimeID, "mcp")
	return nil
}
