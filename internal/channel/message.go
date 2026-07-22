package channel

import (
	cadapter "github.com/polarisagi/polaris/internal/channel/adapter"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"

	"encoding/json"
	"net/http"
	"strconv"
)

// ExtractMessage 将各平台的 webhook payload 统一映射为系统内 ChannelMessage。
// 这是与各平台 API 对接的入站适配层。
func ExtractMessage(channelType string, body []byte, r *http.Request) protocol.ChannelMessage {
	if a, ok := cadapter.Lookup(channelType); ok {
		return a.Extract(body, r)
	}

	switch channelType {
	case "qqbot":
		return extractQQBotWebhook(body)
	case "whatsapp":
		return extractWhatsAppWebhook(body)
	case "sms":
		return extractTwilioWebhook(r)
	case "teams":
		return extractTeamsWebhook(body)
	case "webhook":
		return extractGenericWebhook(body)
	}
	return protocol.ChannelMessage{}
}







func extractQQBotWebhook(body []byte) protocol.ChannelMessage {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return protocol.ChannelMessage{}
	}
	text, _ := raw["content"].(string)
	channelID, _ := raw["channel_id"].(string)
	author, _ := raw["author"].(map[string]any)
	userID, _ := author["id"].(string)
	return protocol.ChannelMessage{Text: text, ChatID: channelID, UserID: userID, TaintLevel: types.TaintHigh}
}

func extractWhatsAppWebhook(body []byte) protocol.ChannelMessage {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return protocol.ChannelMessage{}
	}
	entry, _ := raw["entry"].([]any)
	if len(entry) == 0 {
		return protocol.ChannelMessage{}
	}
	e, _ := entry[0].(map[string]any)
	changes, _ := e["changes"].([]any)
	if len(changes) == 0 {
		return protocol.ChannelMessage{}
	}
	ch, _ := changes[0].(map[string]any)
	value, _ := ch["value"].(map[string]any)
	messages, _ := value["messages"].([]any)
	if len(messages) == 0 {
		return protocol.ChannelMessage{}
	}
	m, _ := messages[0].(map[string]any)
	msgType, _ := m["type"].(string)
	if msgType != "text" {
		return protocol.ChannelMessage{}
	}
	textObj, _ := m["text"].(map[string]any)
	text, _ := textObj["body"].(string)
	from, _ := m["from"].(string)
	return protocol.ChannelMessage{Text: text, ChatID: from, UserID: from, TaintLevel: types.TaintHigh}
}

// extractTwilioWebhook 解析 Twilio 入站 SMS（application/x-www-form-urlencoded）。
func extractTwilioWebhook(r *http.Request) protocol.ChannelMessage {
	if r == nil {
		return protocol.ChannelMessage{}
	}
	if err := r.ParseForm(); err != nil {
		return protocol.ChannelMessage{}
	}
	text := r.FormValue("Body")
	from := r.FormValue("From")
	if text == "" || from == "" {
		return protocol.ChannelMessage{}
	}
	return protocol.ChannelMessage{Text: text, ChatID: from, UserID: from, TaintLevel: types.TaintHigh}
}

// extractTeamsWebhook 解析 MS Teams / MS Graph 变更通知。
func extractTeamsWebhook(body []byte) protocol.ChannelMessage {
	var raw struct {
		Value []struct {
			ResourceData struct {
				Body struct {
					Content string `json:"content"`
				} `json:"body"`
				From struct {
					User struct {
						ID          string `json:"id"`
						DisplayName string `json:"displayName"`
					} `json:"user"`
				} `json:"from"`
				ChatID string `json:"chatId"`
			} `json:"resourceData"`
		} `json:"value"`
	}
	if json.Unmarshal(body, &raw) != nil || len(raw.Value) == 0 {
		return protocol.ChannelMessage{}
	}
	rd := raw.Value[0].ResourceData
	text := rd.Body.Content
	chatID := rd.ChatID
	userID := rd.From.User.ID
	if text == "" || chatID == "" {
		return protocol.ChannelMessage{}
	}
	return protocol.ChannelMessage{Text: text, ChatID: chatID, UserID: userID, TaintLevel: types.TaintHigh}
}

func extractGenericWebhook(body []byte) protocol.ChannelMessage {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return protocol.ChannelMessage{}
	}
	text, _ := raw["content"].(string)
	return protocol.ChannelMessage{Text: text, ChatID: "webhook", TaintLevel: types.TaintHigh}
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
