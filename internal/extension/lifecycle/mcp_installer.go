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

	// 检查是否声明了知识源能力
	for _, cap := range mcpCfg.Capabilities {
		if cap == "knowledge-source" {
			// [硬拦截] connector.MCPKnowledgeConnector 的 List/Fetch/Watch 目前均为
			// CodeUnimplemented 桩实现（MCP resources/list、resources/read 尚未接入
			// 真实 mcp.MCPClient，见 mcp_connector.go 顶部注释）。若在此处注册，
			// SyncScheduler 会持续调度该 connector 并永久收到 CodeUnimplemented，
			// 陷入指数退避空转（永不成功也永不放弃）。在功能补齐前直接拒绝注册，
			// 比"注册后静默空转"更符合 fail-fast——用户能立即看到日志，而不是
			// 误以为知识源已生效但实际从未真正同步过任何文档。
			slog.Warn("mcp_installer: knowledge-source capability declared but connector implementation is a stub (List/Fetch/Watch unimplemented), skipping registration to avoid permanent retry spin",
				"inst_id", req.InstID, "name", name)
		}
	}

	slog.Info("mcp_installer: MCP server registered",
		"inst_id", req.InstID, "name", name, "transport", mcpCfg.Transport)
	return installDir, nil
}

func (m *MCPInstaller) Uninstall(ctx context.Context, req UninstallReq) error {
	_ = m.extRepo.UninstallCleanup(ctx, "", req.RuntimeID, "mcp")
	return nil
}
