package chat

import "github.com/polarisagi/polaris/internal/gateway/session"

// sseFrame 把领域事件映射为 wire 事件名与载荷（A-03 Step3）；事件名/payload 形状与
// 迁移前各 WriteSSE 直接调用点逐字段对齐。KindToolCall 当前未被编排产出，行为等价于
// KindStatus，保留分支避免未来扩展被静默吞掉。ADR-0116 起 session 事件携带 turn_id。
// ok=false 表示该事件无 wire 对应。由 TurnHub 订阅回放（sse_turn.go）使用。
func sseFrame(ev session.Event) (name string, payload any, ok bool) {
	switch ev.Kind {
	case session.KindDelta:
		return "token", map[string]any{"content": ev.Text}, true
	case session.KindReasoning:
		return "reasoning", map[string]any{"content": ev.Text}, true
	case session.KindStatus, session.KindToolCall:
		return "status", ev.Payload, true
	case session.KindContextWarning:
		return "context_warning", ev.Payload, true
	case session.KindComplete:
		return "complete", ev.Payload, true
	case session.KindError:
		return "error", ev.Payload, true
	case session.KindSystemNotice:
		return "system_notice", ev.Payload, true
	case session.KindSteerAccepted:
		return "steer_accepted", ev.Payload, true
	case session.KindSession:
		return "session", ev.Payload, true
	}
	return "", nil, false
}
