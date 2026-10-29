package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
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

// Install 独立连接器安装：目录内标准 .mcp.json / mcp.json（mcpServers 映射）恰好声明一个服务器，
// 多服务器应以插件分发（插件有 plugin_id 级联卸载，独立连接器按实例一对一）。
func (m *MCPInstaller) Install(ctx context.Context, req InstallReq) (InstallResult, error) {
	installDir := req.LocalPath
	if installDir == "" {
		return InstallResult{}, apperr.New(apperr.CodeInvalidInput, "mcp_installer: LocalPath required")
	}
	servers, diags := pluginspec.ListMCPServersInDir(installDir)
	if len(servers) != 1 {
		return InstallResult{}, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf(
			"mcp_installer: expected exactly one server in .mcp.json, got %d (%s)", len(servers), joinDiagnostics(diags)))
	}
	srv := servers[0]
	row := mcpRowFromSpec(srv, mcpRowParams{ID: req.InstID, Name: srv.Name, CatalogID: req.CatalogID,
		TrustTier: req.TrustTier, Enabled: true})
	if err := m.extRepo.UpsertMCPServer(ctx, row); err != nil {
		return InstallResult{}, apperr.Wrap(apperr.CodeInternal, "mcp_installer: persist server", err)
	}
	if m.mcpConn != nil {
		if err := m.mcpConn.StartFromDB(ctx, req.InstID); err != nil {
			return InstallResult{}, apperr.Wrap(apperr.CodeOf(err), "mcp_installer: start server", err)
		}
		m.registerKnowledgeSource(req.InstID, srv)
	}
	slog.Info("mcp_installer: MCP server registered", "inst_id", req.InstID, "name", srv.Name, "type", srv.Type)
	return InstallResult{Dir: installDir, RuntimeID: req.InstID}, nil
}

// registerKnowledgeSource Polaris 私有能力声明：服务器定义中的 "capabilities" 含 knowledge-source 时
// 注册为知识源连接器（非标准字段经 pluginspec.MCPServer.Extra 原样保留）。
func (m *MCPInstaller) registerKnowledgeSource(serverID string, srv pluginspec.MCPServer) {
	if m.registry == nil {
		return
	}
	var caps []string
	if raw, ok := srv.Extra["capabilities"]; ok {
		if err := json.Unmarshal(raw, &caps); err != nil {
			// L3：私有能力声明格式错误只影响知识源注册，连接器本身已可用；留痕。
			slog.Warn("mcp_installer: capabilities must be a string array, ignored", "server", serverID, "err", err)
			return
		}
	}
	for _, c := range caps {
		if c != "knowledge-source" {
			continue
		}
		knowledgeClient, ok := m.mcpConn.GetClient(serverID).(connector.MCPClient)
		if !ok {
			slog.Warn("mcp_installer: knowledge-source declared but client lacks resources/list+read, skipped", "server", serverID)
			return
		}
		m.registry.Register(connector.NewMCPKnowledgeConnector(serverID, srv.Name, knowledgeClient))
		slog.Info("mcp_installer: knowledge-source MCP server registered with SyncScheduler", "server", serverID)
		return
	}
}

func (m *MCPInstaller) Uninstall(ctx context.Context, req UninstallReq) error {
	// 与上面新增的知识源连接器注册对称：卸载时一并摘除，避免 SyncScheduler
	// 继续调度一个客户端已被拆除的 connector（2026-07-21 随 List/Fetch 接入一并修复）。
	if m.registry != nil {
		m.registry.Unregister(req.InstID)
	}
	serverID := req.RuntimeID
	if serverID == "" {
		serverID = req.InstID // 独立连接器行 ID 即实例 ID
	}
	if m.mcpConn != nil {
		m.mcpConn.Remove(serverID)
	}
	if err := m.extRepo.UninstallCleanup(ctx, "", serverID, string(types.TypeMCP)); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mcp_installer.Uninstall", err)
	}
	return nil
}
