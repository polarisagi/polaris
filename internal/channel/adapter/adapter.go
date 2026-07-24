package adapter

import (
	"sync"

	"github.com/polarisagi/polaris/internal/protocol"
)

// Host 是适配器执行 Send/StartPoller 所需的宿主能力（由 channel.Manager 实现）。
// 复用既有 PollerHost（HTTPClient/OnMessage/RegisterPoller/SafeDialer）并扩展 Send 侧能力。
type Host = protocol.ChannelHost

// Adapter 是单个聊天平台的统一契约。实现放在各平台 <platform>.go。
type Adapter = protocol.ChannelAdapter

// 各平台适配器均为进程内单例，用 sync.OnceValue 懒加载构造（内部无锁、并发安全，
// 符合 internal/ 禁全局可变变量的约束——sync.OnceValue(...) 是 inv_NoGlobalVar
// 的既定豁免类别，语义上是"惰性只读单次计算"而非裸全局可变状态）。
//
// 必须是单例而非每次调用新建实例：此前 GetAdapter 曾退化为 factory-style switch，
// 每次调用 `return &WecomAdapter{}, true` 都构造全新实例——WecomAdapter.wecomSends
// 和 MatrixAdapter.MatrixSender.txnCounter 是跨 StartPoller/Send 调用必须持久的
// 实例状态，工厂模式会导致 Send 读到的永远是空 sync.Map / 归零的 txnCounter，
// wecom 回复静默丢失、matrix txn ID 重复。曾尝试用包级 `var registry map[string]Adapter`
// + init() 自注册解决，但那本身违反了 internal/ 禁全局可变变量的红线（Test_inv_NoGlobalVar
// 会直接拦截），故改用本文件内集中的 sync.OnceValue 单例表。
// 下列每个单例均为 sync.OnceValue 懒加载只读单例、无外部可变状态，是 Test_inv_NoGlobalVar
// 的既定豁免类别；golangci-lint 的 gochecknoglobals 不识别该项目内部约定，需逐行显式 nolint
// （对齐 internal/agent/pool.go headlessPromptGuard 的既有写法）。
var (
	getDingTalkAdapter      = sync.OnceValue(func() Adapter { return &DingTalkAdapter{} })      //nolint:gochecknoglobals // sync.OnceValue 懒加载单例，无外部可变状态
	getDiscordAdapter       = sync.OnceValue(func() Adapter { return &DiscordAdapter{} })       //nolint:gochecknoglobals // sync.OnceValue 懒加载单例，无外部可变状态
	getEmailAdapter         = sync.OnceValue(func() Adapter { return &EmailAdapter{} })         //nolint:gochecknoglobals // sync.OnceValue 懒加载单例，无外部可变状态
	getFeishuAdapter        = sync.OnceValue(func() Adapter { return &FeishuAdapter{} })        //nolint:gochecknoglobals // sync.OnceValue 懒加载单例，无外部可变状态
	getHomeAssistantAdapter = sync.OnceValue(func() Adapter { return &HomeAssistantAdapter{} }) //nolint:gochecknoglobals // sync.OnceValue 懒加载单例，无外部可变状态
	getLineAdapter          = sync.OnceValue(func() Adapter { return &LineAdapter{} })          //nolint:gochecknoglobals // sync.OnceValue 懒加载单例，无外部可变状态
	getMatrixAdapter        = sync.OnceValue(func() Adapter { return &MatrixAdapter{} })        //nolint:gochecknoglobals // sync.OnceValue 懒加载单例，MatrixAdapter 内部状态自带并发保护
	getMattermostAdapter    = sync.OnceValue(func() Adapter { return &MattermostAdapter{} })    //nolint:gochecknoglobals // sync.OnceValue 懒加载单例，无外部可变状态
	getQQBotAdapter         = sync.OnceValue(func() Adapter { return &QQBotAdapter{} })         //nolint:gochecknoglobals // sync.OnceValue 懒加载单例，无外部可变状态
	getSignalAdapter        = sync.OnceValue(func() Adapter { return &SignalAdapter{} })        //nolint:gochecknoglobals // sync.OnceValue 懒加载单例，无外部可变状态
	getSlackAdapter         = sync.OnceValue(func() Adapter { return &SlackAdapter{} })         //nolint:gochecknoglobals // sync.OnceValue 懒加载单例，无外部可变状态
	getSmsAdapter           = sync.OnceValue(func() Adapter { return &SmsAdapter{} })           //nolint:gochecknoglobals // sync.OnceValue 懒加载单例，无外部可变状态
	getTeamsAdapter         = sync.OnceValue(func() Adapter { return &TeamsAdapter{} })         //nolint:gochecknoglobals // sync.OnceValue 懒加载单例，无外部可变状态
	getTelegramAdapter      = sync.OnceValue(func() Adapter { return &TelegramAdapter{} })      //nolint:gochecknoglobals // sync.OnceValue 懒加载单例，无外部可变状态
	getWebhookAdapter       = sync.OnceValue(func() Adapter { return &WebhookAdapter{} })       //nolint:gochecknoglobals // sync.OnceValue 懒加载单例，无外部可变状态
	getWecomAdapter         = sync.OnceValue(func() Adapter { return &WecomAdapter{} })         //nolint:gochecknoglobals // sync.OnceValue 懒加载单例，WecomAdapter.wecomSends 为 sync.Map 自带并发保护
	getWhatsappAdapter      = sync.OnceValue(func() Adapter { return &WhatsappAdapter{} })      //nolint:gochecknoglobals // sync.OnceValue 懒加载单例，无外部可变状态
)

// GetAdapter 按 channelType 返回已注册平台的单例适配器。
//
//nolint:gocyclo // 查表 switch，圈复杂度来自平台数量而非逻辑复杂度
func GetAdapter(channelType string) (Adapter, bool) {
	switch channelType {
	case "dingtalk":
		return getDingTalkAdapter(), true
	case "discord":
		return getDiscordAdapter(), true
	case "email":
		return getEmailAdapter(), true
	case "feishu":
		return getFeishuAdapter(), true
	case "homeassistant":
		return getHomeAssistantAdapter(), true
	case "line":
		return getLineAdapter(), true
	case "matrix":
		return getMatrixAdapter(), true
	case "mattermost":
		return getMattermostAdapter(), true
	case "qqbot":
		return getQQBotAdapter(), true
	case "signal":
		return getSignalAdapter(), true
	case "slack":
		return getSlackAdapter(), true
	case "sms":
		return getSmsAdapter(), true
	case "teams":
		return getTeamsAdapter(), true
	case "telegram":
		return getTelegramAdapter(), true
	case "webhook":
		return getWebhookAdapter(), true
	case "wecom":
		return getWecomAdapter(), true
	case "whatsapp":
		return getWhatsappAdapter(), true
	default:
		return nil, false
	}
}
