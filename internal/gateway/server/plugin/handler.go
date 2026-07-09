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

type Dependencies struct {
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
}

// NewPluginHandler 故意不做构造函数级 fail-closed nil 强制校验，结论与理由见
// chat.NewChatHandler 文档注释 + local_playground/reports/
// phase4-hard-dep-and-deadcode-followup-20260708.md（HTTP 路径有 PanicRecovery
// 中间件兜底，真正的进程级崩溃风险在后台 goroutine，非构造函数）。
func NewPluginHandler(deps Dependencies) *PluginHandler {
	return &PluginHandler{
		ExtRepo:              deps.ExtRepo,
		DB:                   deps.DB,
		HTTPClient:           deps.HTTPClient,
		InstallMgr:           deps.InstallMgr,
		HITLGateway:          deps.HITLGateway,
		ClearToolSchemaCache: deps.ClearToolSchemaCache,
		MCPMgr:               deps.MCPMgr,
		DataDir:              deps.DataDir,
		StartMCPServer:       deps.StartMCPServer,
		SkillReg:             deps.SkillReg,
		ScriptRunner:         deps.ScriptRunner,
		PluginCreator:        deps.PluginCreator,
	}
}
