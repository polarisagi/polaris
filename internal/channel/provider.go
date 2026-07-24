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
//
// （原第 3 项 AuthChecker/ChannelAuthChecker 已于 2026-07-12 移除，见下方说明——
// 渠道权限检查目前无真实调用需求，未接线，按死代码清理，不臆造实现。）
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
