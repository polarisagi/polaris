package channel

import (
	cadapter "github.com/polarisagi/polaris/internal/channel/adapter"
	"github.com/polarisagi/polaris/internal/protocol"

	"net/http"
)

// ExtractMessage 将各平台的 webhook payload 统一映射为系统内 ChannelMessage。
// 这是与各平台 API 对接的入站适配层。
func ExtractMessage(channelType string, body []byte, r *http.Request) protocol.ChannelMessage {
	if a, ok := cadapter.GetAdapter(channelType); ok {
		return a.Extract(body, r)
	}
	return protocol.ChannelMessage{}
}
