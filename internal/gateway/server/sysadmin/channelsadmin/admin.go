// Package channelsadmin 承载聊天平台集成（channels CRUD + webhook 接收/验签 +
// 消息分发）的 HTTP handler，从 sysadmin 包摊平的 channels.go（原 579 行，R7
// 超标）拆出为组合式子包（2026-07-07），沿用 cronadmin/insightsadmin/
// workflowadmin 已验证过的模式：独立结构体 + 消费方定义的最小接口集 + 独立
// 构造函数，父 SysAdminHandler 只持有子结构体指针并做单行转发。
package channelsadmin

import (
	"context"
	"net/http"

	cadapter "github.com/polarisagi/polaris/internal/channel/adapter"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

// ChatDispatcher channelsadmin 消费方视角的最小会话接口。
type ChatDispatcher interface {
	EnsureSession(ctx context.Context, sessionID string) error
	SaveMessage(ctx context.Context, sessionID, role, content, toolCalls, reasoningContent string, toolCount int64) error
	UpdateSessionTitle(ctx context.Context, sessionID, firstMessage string) error
	TouchSession(ctx context.Context, sessionID string) error
	ListMessages(ctx context.Context, sessionID string) ([]types.Message, error)
}

// LLMRegistry channelsadmin 消费方视角的最小 Provider 选择接口。
type LLMRegistry interface {
	PickProvider(role string) protocol.Provider
}

// HookFirer channelsadmin 消费方视角的最小 Hook 触发接口
// （sysadmin.HookRunner 的子集，结构性满足，避免 channelsadmin → sysadmin 的
// 反向 import 造成包循环）。
type HookFirer interface {
	Fire(event string, env map[string]string)
	FireBefore(event string, env map[string]string) (blocked bool, reason string)
}

// ChannelMgr channelsadmin 消费方视角的最小平台管理接口。
type ChannelMgr interface {
	SendReply(ctx context.Context, channelID string, replyTo string, options map[string]any, srcMsg cadapter.Message, replyText string)
	Start(channelType, channelID string, cfg map[string]any)
	Stop(channelID string)
	ExtractMessage(channelType string, body []byte, r *http.Request) cadapter.Message
}

// WebhookAutomationTrigger 桥接到 cronadmin 的 webhook 触发型 automation
// （channels 收到消息后顺带检查是否有绑定该频道的 automation）。
type WebhookAutomationTrigger interface {
	TriggerWebhookAutomations(ctx context.Context, channelID, text string)
}

// ChannelsAdmin 承载 channels CRUD + webhook 接收/验签 + 消息分发。
type ChannelsAdmin struct {
	DB          protocol.SQLQuerier
	ChannelRepo repo.ChannelRepository
	ChannelMgr  ChannelMgr
	Registry    LLMRegistry
	Chat        ChatDispatcher
	Hooks       HookFirer
	Cron        WebhookAutomationTrigger

	ToolExec         func(ctx context.Context, name string, args []byte) (*types.ToolResult, error)
	BuildToolSchemas func() []types.ToolSchema
}

// NewChannelsAdmin 构造 ChannelsAdmin。
func NewChannelsAdmin(
	db protocol.SQLQuerier,
	channelRepo repo.ChannelRepository,
	channelMgr ChannelMgr,
	registry LLMRegistry,
	chat ChatDispatcher,
	hooks HookFirer,
	cron WebhookAutomationTrigger,
	toolExec func(ctx context.Context, name string, args []byte) (*types.ToolResult, error),
	buildToolSchemas func() []types.ToolSchema,
) *ChannelsAdmin {
	return &ChannelsAdmin{
		DB:               db,
		ChannelRepo:      channelRepo,
		ChannelMgr:       channelMgr,
		Registry:         registry,
		Chat:             chat,
		Hooks:            hooks,
		Cron:             cron,
		ToolExec:         toolExec,
		BuildToolSchemas: buildToolSchemas,
	}
}
