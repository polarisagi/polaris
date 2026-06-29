package channel

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// 本文件声明 channel 包对外部模块的消费端接口（Consumer-side Interfaces）。
//
// channel 包（各平台适配器）需要以下外部能力：
//   1. ChatRepo     — 持久化聊天消息（读取历史上下文）
//   2. AgentInfer   — 将用户消息转发给 Agent 推理
//   3. AuthChecker  — 检查发送方是否有权限使用该渠道
//
// @consumer: channel/manager.go, channel/dispatch.go
// @producer: 各具体模块由 cli.go/bootstrap 注入

// ChatRepo channel 包对聊天消息存储的消费端接口。
// 实现：store/repo.SQLiteChatRepo
// 禁止：channel 直接 import store/repo（防止 L2→L0 直接依赖）
type ChatRepo interface {
	// SaveMessage 持久化一条消息（用户输入或 Agent 回复）。
	SaveMessage(ctx context.Context, msg *types.ChatMessageRow) error
	// LoadHistory 加载指定会话的最近 N 条消息（构建 LLM 上下文窗口）。
	LoadHistory(ctx context.Context, sessionID string, limit int) ([]types.ChatMessageRow, error)
}

// AgentInfer channel 包对 Agent 推理能力的消费端接口。
// 实现：agent.Agent（通过 DependencyMap["AgentFacade"] 注入）
// 作用：channel 收到用户消息后转给 Agent 推理，推理结果通过回调写回渠道。
type AgentInfer interface {
	// HandleMessage 异步处理一条用户消息，推理完成后通过 reply 回调返回结果。
	// reply 在 Agent goroutine 中调用，调用方需保证线程安全。
	HandleMessage(ctx context.Context, sessionID, userID, text string, taint types.TaintLevel, reply func(string) error)
}

// ChannelAuthChecker channel 包对权限检查的消费端接口。
// 实现：security.SecurityFacade（IsAuthorized）
type ChannelAuthChecker interface {
	// IsChannelAllowed 检查指定用户是否允许通过该渠道发送消息。
	IsChannelAllowed(ctx context.Context, userID, channelType string) (bool, error)
}
