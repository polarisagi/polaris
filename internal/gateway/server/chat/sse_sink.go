package chat

import (
	"net/http"

	"github.com/polarisagi/polaris/internal/gateway/session"
)

// sseSink 实现 session.Sink，把 session.Event 领域事件翻译为既有 SSE wire
// 协议帧（A-03 Step3）。事件名/payload 形状与迁移前 sse.go/sse_stream_helpers.go/
// slash_commands.go 内各处 WriteSSE(w, flusher, "<event>", ...) 直接调用点
// 逐字段对齐，前端（web/ app.js _onEvent）零改动：
//
//	KindDelta          → "token"           {"content": ev.Text}
//	KindReasoning      → "reasoning"        {"content": ev.Text}
//	KindStatus         → "status"           ev.Payload 透传（含既有 type 字段）
//	KindContextWarning → "context_warning"  ev.Payload 透传
//	KindComplete       → "complete"         ev.Payload 透传
//	KindError          → "error"            ev.Payload 透传（{"code","message"}）
//	KindSystemNotice   → "system_notice"    ev.Payload 透传
//	KindToolCall       → 当前编排逻辑未产出（工具调用状态经 KindStatus{type:
//	                     "tool_call"/"tool_result"} 传递），保留分支占位避免
//	                     默认 case 静默吞掉未来扩展，行为等价于 KindStatus。
//
// Emit 始终返回 nil：与迁移前 chat.WriteSSE（内部 fmt.Fprintf 结果被忽略）
// 行为一致——客户端断连的检测走 ctx.Done()（session.orchestrator 内部 FSM
// 事件循环 select 分支），不依赖 Write 调用的返回值，纯搬运不引入新的检测
// 路径。
type sseSink struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func newSSESink(w http.ResponseWriter, flusher http.Flusher) *sseSink {
	return &sseSink{w: w, flusher: flusher}
}

func (s *sseSink) Emit(ev session.Event) error {
	switch ev.Kind {
	case session.KindDelta:
		WriteSSE(s.w, s.flusher, "token", map[string]any{"content": ev.Text})
	case session.KindReasoning:
		WriteSSE(s.w, s.flusher, "reasoning", map[string]any{"content": ev.Text})
	case session.KindStatus, session.KindToolCall:
		WriteSSE(s.w, s.flusher, "status", ev.Payload)
	case session.KindContextWarning:
		WriteSSE(s.w, s.flusher, "context_warning", ev.Payload)
	case session.KindComplete:
		WriteSSE(s.w, s.flusher, "complete", ev.Payload)
	case session.KindError:
		WriteSSE(s.w, s.flusher, "error", ev.Payload)
	case session.KindSystemNotice:
		WriteSSE(s.w, s.flusher, "system_notice", ev.Payload)
	}
	return nil
}
