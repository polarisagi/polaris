package adapter

import (
	"context"
	"net/http"

	"github.com/polarisagi/polaris/internal/protocol"
)

// Host 是适配器执行 Send/StartPoller 所需的宿主能力（由 channel.Manager 实现）。
// 复用既有 PollerHost（HTTPClient/OnMessage/RegisterPoller/SafeDialer）并扩展 Send 侧能力。
type Host interface {
	PollerHost
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

//nolint:gocyclo // Registry factory pattern requires high complexity
func GetAdapter(channelType string) (Adapter, bool) {
	switch channelType {
	case "dingtalk":
		return &DingTalkAdapter{}, true
	case "discord":
		return &DiscordAdapter{}, true
	case "email":
		return &EmailAdapter{}, true
	case "feishu":
		return &FeishuAdapter{}, true
	case "homeassistant":
		return &HomeAssistantAdapter{}, true
	case "line":
		return &LineAdapter{}, true
	case "matrix":
		return &MatrixAdapter{}, true
	case "mattermost":
		return &MattermostAdapter{}, true
	case "qqbot":
		return &QQBotAdapter{}, true
	case "signal":
		return &SignalAdapter{}, true
	case "slack":
		return &SlackAdapter{}, true
	case "sms":
		return &SmsAdapter{}, true
	case "teams":
		return &TeamsAdapter{}, true
	case "telegram":
		return &TelegramAdapter{}, true
	case "webhook":
		return &WebhookAdapter{}, true
	case "wecom":
		return &WecomAdapter{}, true
	case "whatsapp":
		return &WhatsappAdapter{}, true
	default:
		return nil, false
	}
}

func Registered() []string {
	return []string{
		"dingtalk", "discord", "email", "feishu", "homeassistant", "line",
		"matrix", "mattermost", "qqbot", "signal", "slack", "sms", "teams",
		"telegram", "webhook", "wecom", "whatsapp",
	}
}
