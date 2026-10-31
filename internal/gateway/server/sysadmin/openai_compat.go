package sysadmin

// POST /v1/chat/completions — OpenAI API 兼容端点。
//
// 允许任何 OpenAI SDK / 第三方客户端直接对接 Polaris 推理层，
// 无需修改客户端代码。对外协议遵循 OpenAI Chat Completions API v1。
//
// 仅实现 text 消息（role=user/assistant/system）+ stream/non-stream 两种模式。
// tool_use、function_calling、vision 等扩展能力留待 Tier-1+ 实现。

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// ── 请求/响应结构（OpenAI 协议格式）────────────────────────────────────────

type oaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type oaiCompletionReq struct {
	Model       string       `json:"model"`
	Messages    []oaiMessage `json:"messages"`
	Stream      bool         `json:"stream"`
	MaxTokens   int          `json:"max_tokens,omitempty"`
	Temperature float64      `json:"temperature,omitempty"`
}

// oaiChunk SSE 流式 chunk（stream=true 时使用）
type oaiChunk struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []oaiChoice `json:"choices"`
}

type oaiChoice struct {
	Index        int         `json:"index"`
	Delta        oaiDelta    `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
	Message      *oaiMessage `json:"message,omitempty"`
}

type oaiDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// oaiCompletion 非流式完整响应（stream=false 时使用）
type oaiCompletion struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []oaiChoice `json:"choices"`
	Usage   oaiUsage    `json:"usage"`
}

type oaiUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type oaiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

func writeOAIError(w http.ResponseWriter, status int, message, errorType string) {
	httputil.WriteJSONStatus(w, status, map[string]any{
		"error": oaiError{Message: message, Type: errorType},
	})
}

func writeOAIStreamError(w http.ResponseWriter, flusher http.Flusher, message string) {
	payload, err := json.Marshal(map[string]any{"error": oaiError{Message: message, Type: "server_error"}})
	if err != nil {
		return
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload); err != nil {
		slog.Warn("openai compat stream error frame write failed", "error_type", fmt.Sprintf("%T", err))
		return
	}
	flusher.Flush()
}

// ── 处理器 ──────────────────────────────────────────────────────────────────

// HandleOpenAIChat POST /v1/chat/completions
func (h *SysAdminHandler) HandleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	var req oaiCompletionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOAIError(w, http.StatusBadRequest, "Invalid request body", "invalid_request_error")
		return
	}
	if len(req.Messages) == 0 {
		writeOAIError(w, http.StatusBadRequest, "Messages are required", "invalid_request_error")
		return
	}

	// ADR-0094 决策二类推（fail-closed 空依赖判空）：SysAdminHandler 若未被正确
	// 注入 Registry/Router（组件解耦测试、降级运行模式），直接调用会 nil 指针崩溃。
	if h.Registry == nil || h.Router == nil {
		writeOAIError(w, http.StatusServiceUnavailable, "Inference backend is unavailable", "server_error")
		return
	}

	// 选取 Provider（优先 default，次选 general）
	p := h.Registry.PickProvider("default")
	if p == nil {
		p = h.Registry.PickProvider("general")
	}
	if p == nil {
		writeOAIError(w, http.StatusServiceUnavailable, "No inference provider is configured", "server_error")
		return
	}

	// 将 OAI Messages 转换为 types.Message
	msgs := make([]types.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, types.Message{Role: m.Role, Content: m.Content})
	}

	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}
	temp := req.Temperature
	if temp == 0 {
		temp = 0.7
	}

	inferReq := &types.InferRequest{
		Messages:    msgs,
		MaxTokens:   maxTokens,
		Temperature: temp,
	}

	completionID := "chatcmpl-" + oaiRandID()
	modelName := req.Model
	if modelName == "" {
		modelName = "polaris"
	}
	created := time.Now().Unix()

	if req.Stream {
		h.HandleOpenAIChatStream(w, r, p, inferReq, completionID, modelName, created)
	} else {
		h.HandleOpenAIChatSync(w, r, p, inferReq, completionID, modelName, created)
	}
}

// HandleOpenAIChatStream 流式响应（text/event-stream SSE）
func (h *SysAdminHandler) HandleOpenAIChatStream(w http.ResponseWriter, r *http.Request, p protocol.Provider,
	inferReq *types.InferRequest, id, model string, created int64) {

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOAIError(w, http.StatusInternalServerError, "Streaming is unavailable", "server_error")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	ctx := r.Context()

	// 发送 role 帧
	h.writeOAIChunk(w, flusher, oaiChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []oaiChoice{{Index: 0, Delta: oaiDelta{Role: "assistant"}}},
	})

	ch, err := h.Router.StreamInferWithTarget(ctx, p, "openai-compat", inferReq.Messages)
	if err != nil {
		slog.Error("openai compat stream infer failed", "err", err)
		writeOAIStreamError(w, flusher, "Inference request failed")
		return
	}

	idleTimeout := h.StreamIdleTimeout
	if idleTimeout == 0 {
		idleTimeout = 60 * time.Second
	}
	timer := time.NewTimer(idleTimeout)
	defer timer.Stop()

	for {
		timer.Reset(idleTimeout)
		select {
		case ev, ok := <-ch:
			if !ok {
				// 流结束，发送 finish_reason=stop
				stop := "stop"
				h.writeOAIChunk(w, flusher, oaiChunk{
					ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
					Choices: []oaiChoice{{Index: 0, Delta: oaiDelta{}, FinishReason: &stop}},
				})
				fmt.Fprintf(w, "data: [DONE]\n\n")
				flusher.Flush()
				return
			}
			if ev.Type == types.StreamTextDelta && ev.Content != "" {
				h.writeOAIChunk(w, flusher, oaiChunk{
					ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
					Choices: []oaiChoice{{Index: 0, Delta: oaiDelta{Content: ev.Content}}},
				})
			} else if ev.Type == types.StreamError {
				writeOAIStreamError(w, flusher, "Inference request failed")
				return
			} else if ev.Type == types.StreamCancelled {
				// 流被中断（ctx 超时/取消或预算守卫硬阻断）：此前无分支处理，
				// 直接落进下一次循环的 !ok 分支发 finish_reason=stop，外部
				// OpenAI 兼容客户端看到的是"正常结束但内容为空"，无法区分
				// 成功空响应与故障中断。
				writeOAIStreamError(w, flusher, "Inference request timed out")
				return
			}
		case <-timer.C:
			// OpenAI 客户端只把顶层 error 对象识别为失败，不能用普通文本 chunk 伪装成功。
			writeOAIStreamError(w, flusher, "Inference request timed out")
			return
		case <-ctx.Done():
			return
		}
	}
}

// HandleOpenAIChatSync 非流式响应（一次性 JSON）
func (h *SysAdminHandler) HandleOpenAIChatSync(w http.ResponseWriter, r *http.Request, p protocol.Provider,
	inferReq *types.InferRequest, id, model string, created int64) {

	// 使用 StreamInfer 逐 chunk 收集完整内容（与流式路径复用同一 Provider 实现）
	ctx := r.Context()
	ch, err := h.Router.StreamInferWithTarget(ctx, p, "openai-compat", inferReq.Messages)
	if err != nil {
		slog.Error("openai compat sync infer failed", "err", err)
		writeOAIError(w, http.StatusInternalServerError, "Inference request failed", "server_error")
		return
	}

	var sb strings.Builder
	var usage types.Usage
	for ev := range ch {
		switch ev.Type {
		case types.StreamTextDelta:
			sb.WriteString(ev.Content)
			if ev.Usage.InputTokens > 0 || ev.Usage.OutputTokens > 0 {
				usage = ev.Usage
			}
		case types.StreamError:
			writeOAIError(w, http.StatusInternalServerError, "Inference request failed", "server_error")
			return
		case types.StreamCancelled:
			// 流中断（ctx 超时/取消或预算守卫硬阻断）：此前无分支处理，for 循环
			// 结束后径直拿累计的空 sb 拼 200 响应，外部客户端拿到的是一条
			// "生成成功但内容为空"的合法 completion，无法区分真实空回复与故障。
			writeOAIError(w, http.StatusGatewayTimeout, "Inference request timed out", "server_error")
			return
		}
	}

	stop := "stop"
	resp := oaiCompletion{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []oaiChoice{{
			Index:        0,
			FinishReason: &stop,
			Message:      &oaiMessage{Role: "assistant", Content: sb.String()},
		}},
		Usage: oaiUsage{
			PromptTokens:     usage.InputTokens,
			CompletionTokens: usage.OutputTokens,
			TotalTokens:      usage.InputTokens + usage.OutputTokens,
		},
	}

	httputil.WriteJSON(w, resp)
}

func (h *SysAdminHandler) writeOAIChunk(w http.ResponseWriter, flusher http.Flusher, chunk oaiChunk) {
	b, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", b)
	flusher.Flush()
}

func oaiRandID() string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 16)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}
