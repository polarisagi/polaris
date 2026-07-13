package channel

import (
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"

	"encoding/json"
	"net/http"
	"strconv"
)

// ExtractMessage 从各平台 webhook payload 中提取消息内容。
func ExtractMessage(channelType string, body []byte, r *http.Request) protocol.ChannelMessage {
	switch channelType {
	case "telegram":
		return extractTelegramWebhook(body)
	case "discord":
		return extractDiscordWebhook(body)
	case "slack":
		return extractSlackWebhook(body)
	case "feishu":
		return extractFeishuWebhook(body)
	case "line":
		return extractLineWebhook(body)
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

func extractTelegramWebhook(body []byte) protocol.ChannelMessage {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return protocol.ChannelMessage{}
	}
	msg, ok := raw["message"].(map[string]any)
	if !ok {
		return protocol.ChannelMessage{}
	}
	text, _ := msg["text"].(string)
	chatID := jsonNestedInt64(msg, "chat", "id")
	userID := jsonNestedInt64(msg, "from", "id")
	return protocol.ChannelMessage{Text: text, ChatID: chatID, UserID: userID, TaintLevel: types.TaintHigh}
}

func extractDiscordWebhook(body []byte) protocol.ChannelMessage {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return protocol.ChannelMessage{}
	}
	text, _ := raw["content"].(string)
	channelID, _ := raw["channel_id"].(string)
	author, _ := raw["author"].(map[string]any)
	userID, _ := author["id"].(string)
	bot, _ := author["bot"].(bool)
	if bot {
		return protocol.ChannelMessage{}
	}
	return protocol.ChannelMessage{Text: text, ChatID: channelID, UserID: userID, TaintLevel: types.TaintHigh}
}

func extractSlackWebhook(body []byte) protocol.ChannelMessage {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return protocol.ChannelMessage{}
	}
	if ev, ok := raw["event"].(map[string]any); ok {
		text, _ := ev["text"].(string)
		chatID, _ := ev["channel"].(string)
		userID, _ := ev["user"].(string)
		botID, _ := ev["bot_id"].(string)
		if botID != "" {
			return protocol.ChannelMessage{}
		}
		return protocol.ChannelMessage{Text: text, ChatID: chatID, UserID: userID, TaintLevel: types.TaintHigh}
	}
	return protocol.ChannelMessage{}
}

func extractFeishuWebhook(body []byte) protocol.ChannelMessage {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return protocol.ChannelMessage{}
	}
	if ev, ok := raw["event"].(map[string]any); ok { //nolint:nestif
		if m, ok := ev["message"].(map[string]any); ok {
			if content, ok := m["content"].(string); ok {
				var c map[string]any
				if json.Unmarshal([]byte(content), &c) == nil {
					text, _ := c["text"].(string)
					chatID, _ := m["chat_id"].(string)
					senderMap, _ := ev["sender"].(map[string]any)
					senderID, _ := senderMap["sender_id"].(map[string]any)
					openID, _ := senderID["open_id"].(string)
					return protocol.ChannelMessage{Text: text, ChatID: chatID, UserID: openID, TaintLevel: types.TaintHigh}
				}
			}
		}
	}
	return protocol.ChannelMessage{}
}

func extractLineWebhook(body []byte) protocol.ChannelMessage {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return protocol.ChannelMessage{}
	}
	events, _ := raw["events"].([]any)
	if len(events) == 0 {
		return protocol.ChannelMessage{}
	}
	ev, _ := events[0].(map[string]any)
	evType, _ := ev["type"].(string)
	if evType != "message" {
		return protocol.ChannelMessage{}
	}
	msgObj, _ := ev["message"].(map[string]any)
	msgType, _ := msgObj["type"].(string)
	if msgType != "text" {
		return protocol.ChannelMessage{}
	}
	text, _ := msgObj["text"].(string)
	src, _ := ev["source"].(map[string]any)
	chatID := ""
	if groupID, ok := src["groupId"].(string); ok && groupID != "" {
		chatID = groupID
	} else if userID, ok := src["userId"].(string); ok {
		chatID = userID
	}
	replyToken, _ := ev["replyToken"].(string)
	userID, _ := src["userId"].(string)
	return protocol.ChannelMessage{Text: text, ChatID: chatID, UserID: userID, ReplyToken: replyToken, TaintLevel: types.TaintHigh}
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
