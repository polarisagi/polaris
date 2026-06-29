package channel

import (
	"context"

	"github.com/polarisagi/polaris/internal/channel/adapter"
)

// ChannelFacade 聊天平台适配器模块对外统一接口。
//
// 问题背景：
//
//	当前 channel.Manager 对外暴露了 Start/Stop/LoadFromDB/HTTPClient 等方法，
//	上层代码（gateway/server）直接持有 *Manager struct，调用方需了解 Manager 内部细节。
//
// 解决方案：
//   - ChannelFacade 是 channel 包对外的统一入口接口
//   - 上层模块（gateway/server、automation）依赖此接口，不直接持有 *Manager
//   - 支持 TG/Discord/Slack/WeCom/DingTalk 等多平台，调用方无感知具体适配器
//
// @consumer: gateway/server/server.go, automation/scheduler.go
// @producer: channel.Manager（由 cli.go/bootstrap 构造注入）
type ChannelFacade interface {
	// Start 启动指定渠道的消息轮询（已存在时幂等）。
	// channelType: "telegram" | "discord" | "slack" | "wecom" | "dingtalk" 等
	Start(channelID, channelType string, cfg map[string]any)

	// Stop 停止指定渠道的轮询（幂等）。
	Stop(channelID string)

	// StopAll 停止所有渠道轮询（优雅退出时调用）。
	StopAll()

	// Send 向指定渠道发送消息（支持文本/图片/Markdown）。
	Send(ctx context.Context, channelID string, msg adapter.Message) error

	// ActiveChannels 返回当前活跃的渠道 ID 列表。
	ActiveChannels() []string
}
