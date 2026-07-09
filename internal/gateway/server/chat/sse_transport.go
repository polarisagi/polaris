package chat

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

func WriteSSE(w http.ResponseWriter, flusher http.Flusher, eventType string, payload any) {
	data, _ := json.Marshal(payload)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, data)
	flusher.Flush()
}

func (s *ChatHandler) WriteSSEError(w http.ResponseWriter, flusher http.Flusher, code, message string, sessionID string, err error) {
	if code == "hook_blocked" || code == "empty_response" || code == "no_provider" {
		slog.Warn("server: sse error", "code", code, "session", sessionID, "message", message, "err", err)
	} else {
		slog.Error("server: sse error", "code", code, "session", sessionID, "message", message, "err", err)
	}
	WriteSSE(w, flusher, "error", map[string]string{
		"code":    code,
		"message": message,
	})
}

// handleAgentStream 处理 SSE 方式的流式对话。
// 将用户输入包装后转发给 Agent FSM，并订阅 FSM 产生的事件流推送到客户端。
//
// SSE 事件协议（与前端 app.js _onEvent 对齐）:
//
//	thinking  → {"content":"..."} 占位思考指示
//	token     → {"content":"<增量文本>"}
//	complete  → {"session_id":"<id>"}
//	error     → {"code":"...","message":"..."}
//
// sseImagePart 前端上传的图片载荷（base64 字符串，不含 data URI 前缀）。
type sseImagePart struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"` // 纯 base64，不含 "data:...;base64," 前缀
}

type sseAttachment struct {
	URI      string `json:"uri"`
	MimeType string `json:"mime_type"`
	Name     string `json:"name"`
	Data     string `json:"data,omitempty"` // legacy Base64 for backwards compatibility
}
