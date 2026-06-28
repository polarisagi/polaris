package lifecycle

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type MCPInstaller struct {
	extRepo protocol.ExtensionRepository
	mcpConn MCPConnector
}

func NewMCPInstaller(extRepo protocol.ExtensionRepository, mcpConn MCPConnector) *MCPInstaller {
	return &MCPInstaller{
		extRepo: extRepo,
		mcpConn: mcpConn,
	}
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
		Name      string            `json:"name"`
		Transport string            `json:"transport"`
		Command   string            `json:"command"`
		Args      []string          `json:"args"`
		Endpoint  string            `json:"endpoint"`
		Env       map[string]string `json:"env"`
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

	slog.Info("mcp_installer: MCP server registered",
		"inst_id", req.InstID, "name", name, "transport", mcpCfg.Transport)
	return installDir, nil
}

func (m *MCPInstaller) Uninstall(ctx context.Context, req UninstallReq) error {
	_ = m.extRepo.UninstallCleanup(ctx, "", req.RuntimeID, "mcp")
	return nil
}
