package adapter

import (
	"context"
	"github.com/polarisagi/polaris/internal/protocol"
	"net/http"
)

// Host 是适配器执行 Send/StartPoller 所需的宿主能力（由 channel.Manager 实现）。
// 复用既有 PollerHost（HTTPClient/OnMessage/RegisterPoller/SafeDialer）并扩展 Send 侧能力。
type Host interface {
	PollerHost
	// WecomEnqueue 将 wecom 回复投递到 Manager 持有的发送通道；非 wecom 适配器不调用。
	WecomEnqueue(channelID string, msg WecomSendMsg) bool
}

// Adapter 是单个聊天平台的统一契约。实现放在各平台 <platform>.go，并在 init() 中 Register。
type Adapter interface {
	// Type 返回 channelType 键（如 "telegram"）。
	Type() string
	// Extract 从 webhook body 解析入站消息；纯 poller / 无 webhook 的平台返回零值 ChannelMessage。
	Extract(body []byte, r *http.Request) protocol.ChannelMessage
	// Send 将回复发回平台。cfg 为该 channel 的配置。
	Send(ctx context.Context, host Host, cfg map[string]any, msg protocol.ChannelMessage, text string) error
	// StartPoller 启动 poller；纯 webhook 平台直接返回 false（无 poller）。
	StartPoller(host Host, channelID string, cfg map[string]any) (started bool)
}

var registry = map[string]Adapter{}

// Register 在 init() 注册适配器；重复 Type 直接 panic（编译期即暴露冲突）。
func Register(a Adapter) {
	if _, dup := registry[a.Type()]; dup {
		panic("channel adapter duplicate type: " + a.Type())
	}
	registry[a.Type()] = a
}

func Lookup(channelType string) (Adapter, bool) { a, ok := registry[channelType]; return a, ok }
func Registered() map[string]Adapter            { return registry }
