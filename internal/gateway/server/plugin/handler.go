package plugin

import (
	"context"
	"net/http"

	"github.com/polarisagi/polaris/internal/action/hook"
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
	// StartMCPServer 按 mcp_servers 行 ID 启动连接（MCPManager.StartFromDB），行是配置权威源。
	StartMCPServer func(ctx context.Context, serverID string) error
	SkillReg       protocol.SkillRegistry
	PluginCreator  PluginGenerator
	PluginConfig   PluginConfigManager
	// HookRunner hooks.json 引擎（来源审阅 / 信任管理，ADR-0103 决策六）。
	HookRunner *hook.Runner
	// AgentDefs 子 Agent 定义来源（插件 agents/、项目/用户 agents 目录，ADR-0103 决策三）。
	AgentDefs AgentDefinitionLister
	// Channels Claude 插件 channels 启用管理（ADR-0103 决策三）。
	Channels PluginChannelManager

	// EmbeddingIndexer 市场同步后触发的向量预计算器（可 nil，禁用时降级 SQLite LIKE）。
	EmbeddingIndexer *EmbeddingIndexer
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
	StartMCPServer       func(ctx context.Context, serverID string) error
	SkillReg             protocol.SkillRegistry
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
		PluginCreator:        deps.PluginCreator,
	}
}
