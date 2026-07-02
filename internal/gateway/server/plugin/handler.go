package plugin

import (
	"context"
	"net/http"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/gateway/types"
	"github.com/polarisagi/polaris/internal/protocol"
)

// PluginHandler 插件生命周期域依赖。
type PluginHandler struct {
	ExtRepo              protocol.ExtensionRepository
	DB                   protocol.SQLQuerier
	HTTPClient           *http.Client
	InstallMgr           ExtensionInstaller
	HITLGateway          protocol.HITL
	ClearToolSchemaCache func()
	MCPMgr               MCPManager
	DataDir              string
	StartMCPServer       func(ctx context.Context, cfg types.MCPServerConfig) error
	SkillReg             protocol.SkillRegistry
	ScriptRunner         marketplace.HookRunner
	PluginCreator        PluginGenerator

	// EmbeddingIndexer 市场同步后触发的向量预计算器（可 nil，禁用时降级 SQLite LIKE）。
	EmbeddingIndexer *EmbeddingIndexer

	// SyncSkillToToolRegistry 运行时安装插件时，同步自带 skill 到 InMemoryToolRegistry
	SyncSkillToToolRegistry func(slug, instructions string)
}

func NewPluginHandler(
	extRepo protocol.ExtensionRepository,
	db protocol.SQLQuerier,
	httpClient *http.Client,
	installMgr ExtensionInstaller,
	hitlGateway protocol.HITL,
	clearToolSchemaCache func(),
	mcpMgr MCPManager,
	dataDir string,
	startMCPServer func(ctx context.Context, cfg types.MCPServerConfig) error,
	skillReg protocol.SkillRegistry,
	scriptRunner marketplace.HookRunner,
	pluginCreator PluginGenerator,
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
