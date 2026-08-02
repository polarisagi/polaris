package chat

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/gateway/session"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// HandleAgentStream 处理 SSE 方式的流式对话。
//
// [A-03 Step4] 原 281 行的完整会话生命周期编排（会话确保/历史加载/Hook 分发/
// 斜线命令路由/上下文压缩/SystemPromptGuard/消息持久化/Transcript 写入/FSM
// 驱动）已收敛至 session.Orchestrator.RunTurn（见 A-03 Step2）。本函数仅保留
// HTTP 边界层职责：请求解码与校验（含 S-07 的 sessionID 白名单早期拒绝）/
// SSE 响应头与 Flusher/ResponseController 写超时/把 wire 请求翻译为
// session.Request/构造 sseSink（Step3）/调用 RunTurn/错误码映射。
//
// SSE 事件协议（与前端 app.js _onEvent 对齐，逐字段对齐关系见
// chat/sse_sink.go 顶部注释）：
//
//	reasoning → {"content":"..."} 思考过程增量
//	token     → {"content":"..."} 正文增量
//	status    → {"type":"...","message":"..."} 状态提示
//	context_warning → {"usage_percent":...,...} 上下文使用率告警
//	system_notice   → {"message":"...","retry":bool} Agent 池资源降级提示
//	complete  → {"session_id":"...",...}
//	error     → {"code":"...","message":"..."}
func (s *ChatHandler) HandleAgentStream(w http.ResponseWriter, r *http.Request) {
	var req agentStreamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.RespondError(w, "", err, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Input) == "" && len(req.Attachments) == 0 && len(req.ImageParts) == 0 {
		http.Error(w, "input required", http.StatusBadRequest)
		return
	}

	// S-07 入口白名单校验（双重防御第一层，第二层见
	// session.Orchestrator.resolveSessionID/openTranscript）：sessionID 后续
	// 会拼入 transcript 文件路径等场景，未经校验的值可携带 "../" 实现路径穿越。
	// 空值（新会话）由 Orchestrator 内部生成，恒定合法，不在此处校验。
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID != "" && !session.SessionIDPattern.MatchString(sessionID) {
		httputil.RespondError(w, "", apperr.New(apperr.CodeInvalidInput, "invalid session_id"), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // 关闭 nginx 缓冲

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// SSE 长连接：禁用写超时
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	sink := newSSESink(w, flusher)

	if s.Orchestrator == nil {
		sink.Emit(session.Event{ //nolint:errcheck // sseSink.Emit 恒定返回 nil，见 sse_sink.go
			Kind:    session.KindError,
			Payload: map[string]any{"code": "no_orchestrator", "message": "系统错误：会话编排器未初始化"},
		})
		return
	}

	domainReq := session.Request{
		SessionID:       sessionID,
		Input:           req.Input,
		ModelID:         req.ModelID,
		Attachments:     toDomainAttachments(req.Attachments),
		ImageParts:      toDomainImageParts(req.ImageParts),
		Channel:         "web",
		Streaming:       true,
		RunID:           req.RunID,
		ReasoningEffort: req.ReasoningEffort,
	}

	if _, err := s.Orchestrator.RunTurn(r.Context(), domainReq, sink); err != nil {
		// RunTurn 只在会话确保之前的边界校验失败（如非法 session_id 绕过前置
		// 检查）时返回非 nil error；正常业务失败（no_provider/empty_response/
		// hook_blocked 等）均已经 sink 推送 KindError 事件并返回 nil error，
		// 不落入此分支。
		sink.Emit(session.Event{ //nolint:errcheck // sseSink.Emit 恒定返回 nil
			Kind:    session.KindError,
			Payload: map[string]any{"code": "turn_error", "message": err.Error()},
		})
	}
}

// toDomainAttachments 把 wire 协议的 sseAttachment 翻译为 session.Attachment
// （纯格式转译，不含业务决策，留在 HTTP 边界层）。
func toDomainAttachments(atts []sseAttachment) []session.Attachment {
	if len(atts) == 0 {
		return nil
	}
	out := make([]session.Attachment, 0, len(atts))
	for _, a := range atts {
		out = append(out, session.Attachment{
			URI:      a.URI,
			MimeType: a.MimeType,
			Name:     a.Name,
			Data:     a.Data,
		})
	}
	return out
}

// toDomainImageParts 把 wire 协议的 legacy base64 sseImagePart 解码为
// types.ImagePart（纯格式转译：base64 解码不含磁盘 IO/业务决策，留在 HTTP
// 边界层；单条解码失败跳过并计入日志，与原 buildStreamUserMessage 内联逻辑
// 行为等价，见 session.buildUserMessage 注释）。
func toDomainImageParts(parts []sseImagePart) []types.ImagePart {
	if len(parts) == 0 {
		return nil
	}
	out := make([]types.ImagePart, 0, len(parts))
	for _, ip := range parts {
		raw, err := base64.StdEncoding.DecodeString(ip.Data)
		if err != nil {
			continue
		}
		out = append(out, types.ImagePart{
			Type:      "image",
			MediaType: ip.MimeType,
			Data:      raw,
		})
	}
	return out
}
