package channel

import (
	cadapter "github.com/polarisagi/polaris/internal/channel/adapter"
	"github.com/polarisagi/polaris/internal/protocol"

	"net/http"
	"strconv"
)

// ExtractMessage 将各平台的 webhook payload 统一映射为系统内 ChannelMessage。
// 这是与各平台 API 对接的入站适配层。
func ExtractMessage(channelType string, body []byte, r *http.Request) protocol.ChannelMessage {
	if a, ok := cadapter.Lookup(channelType); ok {
		return a.Extract(body, r)
	}
	return protocol.ChannelMessage{}
}

// jsonNestedInt64 从嵌套 map 提取 float64 ID 字段并转字符串。
func jsonNestedInt64(m map[string]any, nested, key string) string {
	sub, ok := m[nested].(map[string]any)
	if !ok {
		return ""
	}
	f, ok := sub[key].(float64)
	if !ok {
		return ""
	}
	return strconv.FormatInt(int64(f), 10)
}
