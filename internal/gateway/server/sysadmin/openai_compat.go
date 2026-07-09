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

// ── 处理器 ──────────────────────────────────────────────────────────────────

// handleOpenAIChat POST /v1/chat/completions
func (h *SysAdminHandler) HandleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	var req oaiCompletionReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":{"message":"invalid request body","type":"invalid_request_error"}}`, http.StatusBadRequest)
		return
	}
	if len(req.Messages) == 0 {
		http.Error(w, `{"error":{"message":"messages is required","type":"invalid_request_error"}}`, http.StatusBadRequest)
		return
	}

	// 选取 Provider（优先 default，次选 general）
	p := h.Registry.PickProvider("default")
	if p == nil {
		p = h.Registry.PickProvider("general")
	}
	if p == nil {
		http.Error(w, `{"error":{"message":"no provider configured","type":"server_error"}}`, http.StatusServiceUnavailable)
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

// handleOpenAIChatStream 流式响应（text/event-stream SSE）
func (h *SysAdminHandler) HandleOpenAIChatStream(w http.ResponseWriter, r *http.Request, p protocol.Provider,
	inferReq *types.InferRequest, id, model string, created int64) {

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
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

	ch, err := p.StreamInfer(ctx, inferReq.Messages)
	if err != nil {
		slog.Error("openai_compat: StreamInfer failed", "err", err)
		h.writeOAIChunk(w, flusher, oaiChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []oaiChoice{{Index: 0, Delta: oaiDelta{Content: "[error: " + truncate(err.Error(), 100) + "]"}}},
		})
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	for {
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
			} else if ev.Type == types.StreamError && ev.Content != "" {
				h.writeOAIChunk(w, flusher, oaiChunk{
					ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
					Choices: []oaiChoice{{Index: 0, Delta: oaiDelta{Content: "[error: " + ev.Content + "]"}}},
				})
			}
		case <-ctx.Done():
			return
		}
	}
}

// handleOpenAIChatSync 非流式响应（一次性 JSON）
func (h *SysAdminHandler) HandleOpenAIChatSync(w http.ResponseWriter, r *http.Request, p protocol.Provider,
	inferReq *types.InferRequest, id, model string, created int64) {

	// 使用 StreamInfer 逐 chunk 收集完整内容（与流式路径复用同一 Provider 实现）
	ctx := r.Context()
	ch, err := p.StreamInfer(ctx, inferReq.Messages)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":{"message":"%s","type":"server_error"}}`, truncate(err.Error(), 200)), http.StatusInternalServerError)
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
			if ev.Content != "" {
				http.Error(w, fmt.Sprintf(`{"error":{"message":"%s","type":"server_error"}}`, truncate(ev.Content, 200)), http.StatusInternalServerError)
				return
			}
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

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
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

func truncate(s string, max int) string {
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
