package chat

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// WriteSSE 是 SSE wire 帧写入原语，供 chat 包内所有直接产出 SSE 响应的场景
// 复用（chat/sse_sink.go 的 session.Sink 实现、audio.go 等）。
//
// [A-03 Step4] ChatHandler.WriteSSEError（原 error 事件写入 + 分级日志封装）
// 已随 HandleAgentStream 瘦身一并移除：唯一调用方是原 281 行版本的
// HandleAgentStream 自身；日志与安全错误构造现由 session.orchestrator.emitError
// 统一处理，SSE 适配器只投影安全 Payload，不序列化 Event.Err。
func WriteSSE(w http.ResponseWriter, flusher http.Flusher, eventType string, payload any) {
	data, _ := json.Marshal(payload)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, data)
	flusher.Flush()
}

// writeSSEWithID 同 WriteSSE，但带 `id: <seq>` 行，供客户端断线后按 after=<seq> 续传。
func writeSSEWithID(w http.ResponseWriter, flusher http.Flusher, seq int64, eventType string, payload any) {
	data, _ := json.Marshal(payload)
	fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", seq, eventType, data)
	flusher.Flush()
}

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
