// Package workflowadmin 承载 workflow（多步骤 Agent 任务编排）的 HTTP handler +
// 执行引擎，从 sysadmin 包摊平的 workflow.go（原 730 行，R7 超标）拆出为组合式
// 子包（2026-07-07），沿用 cronadmin/insightsadmin 已验证过的模式：独立结构体 +
// 消费方定义的最小接口集 + 独立构造函数，父 SysAdminHandler 只持有子结构体指针
// 并做单行转发。
package workflowadmin

import (
	"context"
	"sync"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

// ChatDispatcher workflowadmin 消费方视角的最小会话接口
// （sysadmin.ChatDispatcher 的子集，结构性满足，无需适配器）。
type ChatDispatcher interface {
	EnsureSession(ctx context.Context, sessionID string) error
	InjectSystemPrompt(ctx context.Context, agentCtrl protocol.AgentController, history []types.Message, userQuery string) []types.Message
	SaveMessage(ctx context.Context, sessionID, role, content, toolCalls, reasoningContent string, toolCount int64) error
	UpdateSessionTitle(ctx context.Context, sessionID, firstMessage string) error
}

// WorkflowAdmin 承载 workflow CRUD + cron 触发 + 顺序执行引擎。
type WorkflowAdmin struct {
	DB               protocol.SQLQuerier
	WorkflowRepo     repo.WorkflowRepository
	AgentPool        protocol.AgentPool
	Chat             ChatDispatcher
	TemplateCacheMap *sync.Map

	ToolExec         func(ctx context.Context, name string, args []byte) (*types.ToolResult, error)
	BuildToolSchemas func() []types.ToolSchema
}

// NewWorkflowAdmin 构造 WorkflowAdmin。
func NewWorkflowAdmin(
	db protocol.SQLQuerier,
	r repo.WorkflowRepository,
	agentPool protocol.AgentPool,
	chat ChatDispatcher,
	toolExec func(ctx context.Context, name string, args []byte) (*types.ToolResult, error),
	buildToolSchemas func() []types.ToolSchema,
) *WorkflowAdmin {
	return &WorkflowAdmin{
		DB:               db,
		WorkflowRepo:     r,
		AgentPool:        agentPool,
		Chat:             chat,
		TemplateCacheMap: &sync.Map{},
		ToolExec:         toolExec,
		BuildToolSchemas: buildToolSchemas,
	}
}
