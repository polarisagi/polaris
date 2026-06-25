package plugin

import (
	extplugin "github.com/polarisagi/polaris/internal/extension/plugin"

	"context"
	"net/http"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/internal/gateway/types"
	"github.com/polarisagi/polaris/internal/protocol"
)

// PluginHandler 插件生命周期域依赖。
type PluginHandler struct {
	ExtRepo              protocol.ExtensionRepository
	DB                   protocol.SQLQuerier
	HTTPClient           *http.Client
	InstallMgr           *marketplace.Manager
	HITLGateway          protocol.HITL
	ClearToolSchemaCache func()
	MCPMgr               *mcp.MCPManager
	DataDir              string
	StartMCPServer       func(ctx context.Context, cfg types.MCPServerConfig) error
	SkillReg             protocol.SkillRegistry
	ScriptRunner         marketplace.HookRunner
	PluginCreator        *extplugin.PluginCreator

	// EmbeddingIndexer 市场同步后触发的向量预计算器（可 nil，禁用时降级 SQLite LIKE）。
	EmbeddingIndexer *EmbeddingIndexer
}

func NewPluginHandler(
	extRepo protocol.ExtensionRepository,
	db protocol.SQLQuerier,
	httpClient *http.Client,
	installMgr *marketplace.Manager,
	hitlGateway protocol.HITL,
	clearToolSchemaCache func(),
	mcpMgr *mcp.MCPManager,
	dataDir string,
	startMCPServer func(ctx context.Context, cfg types.MCPServerConfig) error,
	skillReg protocol.SkillRegistry,
	scriptRunner marketplace.HookRunner,
	pluginCreator *extplugin.PluginCreator,
) *PluginHandler {
	return &PluginHandler{
		ExtRepo:              extRepo,
		DB:                   db,
		HTTPClient:           httpClient,
		InstallMgr:           installMgr,
		HITLGateway:          hitlGateway,
		ClearToolSchemaCache: clearToolSchemaCache,
		MCPMgr:               mcpMgr,
		DataDir:              dataDir,
		StartMCPServer:       startMCPServer,
		SkillReg:             skillReg,
		ScriptRunner:         scriptRunner,
		PluginCreator:        pluginCreator,
	}
}
