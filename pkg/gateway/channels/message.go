package channels

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// ExtractMessage 从各平台 webhook payload 中提取消息内容。
func ExtractMessage(channelType string, body []byte, r *http.Request) Message {
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
	return Message{}
}

func extractTelegramWebhook(body []byte) Message {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return Message{}
	}
	msg, ok := raw["message"].(map[string]any)
	if !ok {
		return Message{}
	}
	text, _ := msg["text"].(string)
	chatID := jsonNestedInt64(msg, "chat", "id")
	userID := jsonNestedInt64(msg, "from", "id")
	return Message{Text: text, ChatID: chatID, UserID: userID}
}

func extractDiscordWebhook(body []byte) Message {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return Message{}
	}
	text, _ := raw["content"].(string)
	channelID, _ := raw["channel_id"].(string)
	author, _ := raw["author"].(map[string]any)
	userID, _ := author["id"].(string)
	bot, _ := author["bot"].(bool)
	if bot {
		return Message{}
	}
	return Message{Text: text, ChatID: channelID, UserID: userID}
}

func extractSlackWebhook(body []byte) Message {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return Message{}
	}
	if ev, ok := raw["event"].(map[string]any); ok {
		text, _ := ev["text"].(string)
		chatID, _ := ev["channel"].(string)
		userID, _ := ev["user"].(string)
		botID, _ := ev["bot_id"].(string)
		if botID != "" {
			return Message{}
		}
		return Message{Text: text, ChatID: chatID, UserID: userID}
	}
	return Message{}
}

func extractFeishuWebhook(body []byte) Message {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return Message{}
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
					return Message{Text: text, ChatID: chatID, UserID: openID}
				}
			}
		}
	}
	return Message{}
}

func extractLineWebhook(body []byte) Message {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return Message{}
	}
	events, _ := raw["events"].([]any)
	if len(events) == 0 {
		return Message{}
	}
	ev, _ := events[0].(map[string]any)
	evType, _ := ev["type"].(string)
	if evType != "message" {
		return Message{}
	}
	msgObj, _ := ev["message"].(map[string]any)
	msgType, _ := msgObj["type"].(string)
	if msgType != "text" {
		return Message{}
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
	return Message{Text: text, ChatID: chatID, UserID: userID, ReplyToken: replyToken}
}

func extractQQBotWebhook(body []byte) Message {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return Message{}
	}
	text, _ := raw["content"].(string)
	channelID, _ := raw["channel_id"].(string)
	author, _ := raw["author"].(map[string]any)
	userID, _ := author["id"].(string)
	return Message{Text: text, ChatID: channelID, UserID: userID}
}

func extractWhatsAppWebhook(body []byte) Message {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return Message{}
	}
	entry, _ := raw["entry"].([]any)
	if len(entry) == 0 {
		return Message{}
	}
	e, _ := entry[0].(map[string]any)
	changes, _ := e["changes"].([]any)
	if len(changes) == 0 {
		return Message{}
	}
	ch, _ := changes[0].(map[string]any)
	value, _ := ch["value"].(map[string]any)
	messages, _ := value["messages"].([]any)
	if len(messages) == 0 {
		return Message{}
	}
	m, _ := messages[0].(map[string]any)
	msgType, _ := m["type"].(string)
	if msgType != "text" {
		return Message{}
	}
	textObj, _ := m["text"].(map[string]any)
	text, _ := textObj["body"].(string)
	from, _ := m["from"].(string)
	return Message{Text: text, ChatID: from, UserID: from}
}

// extractTwilioWebhook 解析 Twilio 入站 SMS（application/x-www-form-urlencoded）。
func extractTwilioWebhook(r *http.Request) Message {
	if r == nil {
		return Message{}
	}
	if err := r.ParseForm(); err != nil {
		return Message{}
	}
	text := r.FormValue("Body")
	from := r.FormValue("From")
	if text == "" || from == "" {
		return Message{}
	}
	return Message{Text: text, ChatID: from, UserID: from}
}

// extractTeamsWebhook 解析 MS Teams / MS Graph 变更通知。
func extractTeamsWebhook(body []byte) Message {
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
		return Message{}
	}
	rd := raw.Value[0].ResourceData
	text := rd.Body.Content
	chatID := rd.ChatID
	userID := rd.From.User.ID
	if text == "" || chatID == "" {
		return Message{}
	}
	return Message{Text: text, ChatID: chatID, UserID: userID}
}

func extractGenericWebhook(body []byte) Message {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return Message{}
	}
	text, _ := raw["content"].(string)
	return Message{Text: text, ChatID: "webhook"}
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

// jsonStr 从 map[string]json.RawMessage 提取字符串字段。
func jsonStr(m map[string]json.RawMessage, key string) string {
	if v, ok := m[key]; ok {
		var s string
		_ = json.Unmarshal(v, &s)
		return s
	}
	return ""
}
