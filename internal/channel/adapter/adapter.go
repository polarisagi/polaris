package adapter

import (
	"net/http"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// Host 是适配器执行 Send/StartPoller 所需的宿主能力（由 channel.Manager 实现）。
// 复用既有 PollerHost（HTTPClient/OnMessage/RegisterPoller/SafeDialer）并扩展 Send 侧能力。
type Host = protocol.ChannelHost

// deriveClient 从受保护的 base client（host.HTTPClient()，Transport 挂载
// SafeDialer 五阶段 SSRF 校验）派生一个仅覆盖 Timeout 的新 client（S-03）。
// 只允许覆盖 Timeout，Transport/CheckRedirect/Jar 必须原样继承——SSRF 防护
// 全部挂在 Transport.DialContext 上，新建 Transport 等于绕过防护。
// base 为 nil 时 fail-closed 返回 error，禁止退化为 http.DefaultClient。
func deriveClient(base *http.Client, timeout time.Duration) (*http.Client, error) {
	if base == nil {
		return nil, apperr.New(apperr.CodeInternal, "adapter: protected http client is required (fail-closed)")
	}
	return &http.Client{
		Transport:     base.Transport,
		CheckRedirect: base.CheckRedirect,
		Jar:           base.Jar,
		Timeout:       timeout,
	}, nil
}

// Adapter 是单个聊天平台的统一契约。
type Adapter = protocol.ChannelAdapter

var (
	getWebhookAdapter = sync.OnceValue(func() Adapter { return &WebhookAdapter{} }) //nolint:gochecknoglobals // sync.OnceValue 懒加载单例，无外部可变状态
)

// GetAdapter 按 channelType 返回已注册平台的单例适配器。
func GetAdapter(channelType string) (Adapter, bool) {
	switch channelType {
	case "webhook":
		return getWebhookAdapter(), true
	default:
		return nil, false
	}
}
