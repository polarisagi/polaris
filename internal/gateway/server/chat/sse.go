package chat

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/ffi"
	"github.com/polarisagi/polaris/internal/memory/store"
	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/types"
)

// skillEmbedCache 进程内技能文本 → 向量缓存（sha256(text) 为 key）。
// 技能安装/卸载极低频，进程级 map 足够，无需落盘。
// 上限 skillEmbedCacheMax 条：超限时随机淘汰（技能数量有限，随机淘汰成本低）。
// 容量估算：512 × 1536 维 × 4 字节 ≈ 3MB，可接受。
const skillEmbedCacheMax = 512

var (
	skillEmbedCacheMu sync.RWMutex
	skillEmbedCache   = make(map[string][]float32)
)

func writeSSE(w http.ResponseWriter, flusher http.Flusher, eventType string, payload any) {
	data, _ := json.Marshal(payload)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, data)
	flusher.Flush()
}

func (s *ChatHandler) writeSSEError(w http.ResponseWriter, flusher http.Flusher, code, message string, sessionID string, err error) {
	if code == "hook_blocked" || code == "empty_response" || code == "no_provider" {
		slog.Warn("server: sse error", "code", code, "session", sessionID, "message", message, "err", err)
	} else {
		slog.Error("server: sse error", "code", code, "session", sessionID, "message", message, "err", err)
	}
	writeSSE(w, flusher, "error", map[string]string{
		"code":    code,
		"message": message,
	})
}

// handleAgentStream 处理 SSE 方式的流式对话。
// 直接从 ProviderRegistry 选取最优 Provider 调用 StreamInfer，
// 绕过尚未打通的 FSM→Blackboard 链路（MVP 直通模式）。
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

func (s *ChatHandler) HandleAgentStream(w http.ResponseWriter, r *http.Request) { //nolint:gocyclo
	var req struct {
		Input           string          `json:"input"`
		SessionID       string          `json:"session_id,omitempty"`
		RunID           string          `json:"run_id,omitempty"`
		ModelID         string          `json:"model_id,omitempty"`
		ReasoningEffort string          `json:"reasoning_effort,omitempty"`
		Attachments     []sseAttachment `json:"attachments,omitempty"`
		// back-compat
		ImageParts []sseImagePart `json:"image_parts,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Input) == "" && len(req.Attachments) == 0 && len(req.ImageParts) == 0 {
		http.Error(w, "input required", http.StatusBadRequest)
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

	ctx := r.Context()

	// ── 会话管理 ──────────────────────────────────────────────────────────
	sessionID := strings.TrimSpace(req.SessionID)
	isNewSession := sessionID == ""
	if isNewSession {
		sessionID = newSessionID()
	}
	if err := s.EnsureSession(ctx, sessionID); err != nil {
		s.writeSSEError(w, flusher, "session_error", err.Error(), sessionID, err)
		return
	}
	// session.new hook：用户发起新会话时触发（req.SessionID 为空意味着 /new 后首条消息）
	if isNewSession {
		s.Hooks.Fire("session.new", map[string]string{
			"POLARIS_SESSION_ID": sessionID,
			"POLARIS_CHANNEL":    "web",
		})
	}

	// message.before hook：同步拦截，非零退出 = 拒绝本条消息
	if blocked, reason := s.Hooks.FireBefore("message.before", map[string]string{
		"POLARIS_MESSAGE":    req.Input,
		"POLARIS_SESSION_ID": sessionID,
		"POLARIS_CHANNEL":    "web",
	}); blocked {
		s.writeSSEError(w, flusher, "hook_blocked", reason, sessionID, nil)
		return
	}

	// 加载历史消息（多轮上下文）
	history, err := s.LoadMessages(ctx, sessionID)
	if err != nil {
		s.writeSSEError(w, flusher, "history_error", err.Error(), sessionID, err)
		return
	}
	isFirstTurn := len(history) == 0

	var agentCtrl protocol.AgentController
	if s.AgentPool != nil {
		var release func()
		var err error
		agentCtrl, release, err = s.AgentPool.Acquire(ctx, sessionID)
		if err != nil {
			s.writeSSEError(w, flusher, "agent_pool_error", "failed to acquire agent: "+err.Error(), sessionID, err)
			return
		}
		defer release()
	}

	if len(history) > 0 {
		history = s.InjectSystemPrompt(ctx, agentCtrl, history, req.Input)
	}
	// [新增] 将用户 input 注入 Agent FSM（非阻塞）
	if agentCtrl != nil {
		agentCtrl.SetTaskIntent([]byte(req.Input))
		err := agentCtrl.SendIntent(types.TriggerIntentReceived)
		if err != nil {
			slog.Warn("server: fsm advance failed or timeout", "err", err)
		}
	}

	// ── Transcript ────────────────────────────────────────────────────────
	// 非阻塞：打开失败只警告，不中断对话。
	tw, twErr := openTranscript(s.TranscriptDir, sessionID, isFirstTurn)
	if twErr != nil {
		slog.Warn("server: transcript open failed", "session", sessionID, "err", twErr)
	}
	if tw != nil {
		defer tw.Close()
	}

	// 追加本轮用户消息（含图片 Parts）
	var userPromptBuilder strings.Builder
	userPromptBuilder.WriteString(req.Input)

	var hasMedia bool
	mediaParts := make([]any, 0, len(req.Attachments)+len(req.ImageParts))

	// 处理新增的 VFS 附件
	for _, att := range req.Attachments {
		isImage := strings.HasPrefix(att.MimeType, "image/")
		isVideo := strings.HasPrefix(att.MimeType, "video/")

		if !isImage && !isVideo {
			// 非图片/视频文件，向提示词中注入挂载信息
			fmt.Fprintf(&userPromptBuilder, "\n\n[System: 用户挂载了系统附件 %s", att.URI)
			if att.Name != "" {
				fmt.Fprintf(&userPromptBuilder, " (原始文件名: %s)", att.Name)
			}
			userPromptBuilder.WriteString("]")
			continue
		}

		// 必须是 workspace:// 协议才能读本地文件
		if !strings.HasPrefix(att.URI, "workspace://") {
			slog.Warn("server: non-workspace URI skipped for media attachment", "uri", att.URI)
			continue
		}

		localPath := filepath.Join(s.DataDir, "workspace", strings.TrimPrefix(att.URI, "workspace://"))

		if isVideo {
			// 视频大小门控：超过 Gemini inlineData 上限（20MB）直接拒绝，避免 OOM
			fi, statErr := os.Stat(localPath)
			if statErr != nil {
				slog.Warn("server: failed to stat video attachment", "uri", att.URI, "err", statErr)
				continue
			}
			if fi.Size() > maxVideoInlineBytes {
				slog.Warn("server: video too large for inline, skipping", "uri", att.URI, "size", fi.Size())
				name := att.Name
				if name == "" {
					name = att.URI
				}
				fmt.Fprintf(&userPromptBuilder, "\n\n[System: 视频文件 %s (%.1fMB) 超过内联上限（20MB），未能传递给模型。请使用较小的视频片段。]",
					name, float64(fi.Size())/(1024*1024))
				continue
			}
		}

		raw, err := os.ReadFile(localPath)
		if err != nil {
			slog.Warn("server: failed to read media attachment", "uri", att.URI, "err", err)
			continue
		}

		hasMedia = true
		if isImage {
			// 图片原样构造 ImagePart，压缩/降采样由 InferenceRouter.normalizeInferRequest() 统一处理
			mediaParts = append(mediaParts, types.ImagePart{
				Type:      "image",
				MediaType: att.MimeType,
				Data:      raw,
			})
		} else {
			// video/* → Gemini inlineData 方式（已通过上方大小门控，≤20MB）
			mediaParts = append(mediaParts, types.VideoPart{
				Type:      "video",
				MediaType: att.MimeType,
				Data:      raw,
			})
		}
	}

	finalInput := strings.TrimSpace(userPromptBuilder.String())
	userMsg := types.Message{Role: "user", Content: finalInput}

	// 兼容老版本的 Base64 图片
	if len(req.ImageParts) > 0 {
		for _, ip := range req.ImageParts {
			raw, err := base64.StdEncoding.DecodeString(ip.Data)
			if err != nil {
				slog.Warn("server: invalid image base64, skipping", "err", err)
				continue
			}
			// 图片原样构造 ImagePart，压缩/降采样由 InferenceRouter.normalizeInferRequest() 统一处理
			hasMedia = true
			mediaParts = append(mediaParts, types.ImagePart{
				Type:      "image",
				MediaType: ip.MimeType,
				Data:      raw,
			})
		}
	}

	if hasMedia {
		parts := make([]any, 0, 1+len(mediaParts))
		if finalInput != "" {
			parts = append(parts, map[string]any{"type": "text", "text": finalInput})
		}
		parts = append(parts, mediaParts...)
		userMsg.Parts = parts
	}

	history = append(history, userMsg)
	if err := s.SaveMessage(ctx, sessionID, "user", finalInput, "", 0); err != nil {
		slog.Error("server: saveMessage user", "session", sessionID, "err", err)
	}
	if tw != nil {
		tw.WriteTurn("user", req.Input, 0, 0)
	}

	// ── 选取最优 Provider ─────────────────────────────────────────────────
	var p protocol.Provider
	if req.ModelID != "" {
		p = s.Registry.PickProviderByRecordID(req.ModelID)
	}
	if p == nil {
		// 优先用 "default" 角色（对话模型），次选 "general"（参与全局 LB）
		p = s.Registry.PickProvider("default")
		if p == nil {
			p = s.Registry.PickProvider("general")
		}
	}
	if p == nil {
		if tw != nil {
			tw.WriteError("no_provider", "未配置任何启用的 LLM 厂商")
		}
		s.writeSSEError(w, flusher, "no_provider", "未配置任何启用的 LLM 厂商，请在「模型」页添加并启用厂商", sessionID, nil)
		return
	}

	// ── 斜线命令拦截（短路 LLM 推理）────────────────────────────────────────
	if cmdResult := s.SlashRouter.Dispatch(ctx, finalInput, sessionID, history, p, w, flusher); cmdResult.Handled {
		if cmdResult.Response != "" {
			saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.SaveMessage(saveCtx, sessionID, "assistant", cmdResult.Response, "", 0); err != nil {
				slog.Error("server: saveMessage slash response", "session", sessionID, "err", err)
			}
		}
		_ = s.TouchSession(context.WithoutCancel(ctx), sessionID)
		writeSSE(w, flusher, "complete", map[string]any{"session_id": sessionID, "session_title": ""})
		return
	}

	// ── 上下文使用率评估（警告 + 防抖动告警 + 自动压缩）────────────────────────
	// 阈值模型对齐 Claude Code：contextWindow × autoCompactPct%（默认 95%），
	// warnPct%（默认 80%）时提前告警，连续 thrashing 后停止自动压缩并专项提示。
	ctxStats := s.Compressor.Stats(history)

	if ctxStats.UsagePercent >= s.Compressor.WarnPct() {
		msg := fmt.Sprintf("上下文使用量已达 %d%%，可使用 /compact 手动压缩", int(ctxStats.UsagePercent))
		if ctxStats.Thrashing {
			msg = fmt.Sprintf("⚠ 自动压缩抖动：上下文 %d%% 使用量居高不下，请手动 /compact 并缩减单次工具输出规模", int(ctxStats.UsagePercent))
		}
		writeSSE(w, flusher, "context_warning", map[string]any{
			"usage_percent": int(ctxStats.UsagePercent),
			"token_count":   ctxStats.TokenCount,
			"threshold":     ctxStats.Threshold,
			"thrashing":     ctxStats.Thrashing,
			"message":       msg,
		})
	}

	// 自动压缩：非 thrashing 状态 + 超过 autoCompactPct 阈值 → 静默压缩后继续推理
	if !ctxStats.Thrashing && s.Compressor.NeedsCompact(history) {
		writeSSE(w, flusher, "status", map[string]any{"type": "compacting", "message": "正在压缩上下文..."})
		if compacted, res, err := s.Compressor.Compact(ctx, sessionID, history, p); err == nil && !res.Skipped {
			history = compacted
			writeSSE(w, flusher, "status", map[string]any{
				"type":          "compacted",
				"tokens_before": res.TokensBefore,
				"tokens_after":  res.TokensAfter,
				"message":       fmt.Sprintf("上下文已压缩：%d → %d tokens", res.TokensBefore, res.TokensAfter),
			})
		}
	}

	// ── 推理（含 tool_use 循环，最多 10 轮）────────────────────────────────
	writeSSE(w, flusher, "thinking", map[string]string{"content": "..."})

	// 语义工具选择：当工具数 > toolSelectThreshold 且 Embedder 可用时按 query 相似度过滤到 top-K，
	// 否则退回全量注入。通过接口类型断言实现，不污染 ToolProvider 接口签名。
	toolSchemas := s.ToolProvider.BuildToolSchemas()
	if sel, ok := s.ToolProvider.(interface {
		SelectToolSchemas(string) []types.ToolSchema
	}); ok {
		if picked := sel.SelectToolSchemas(req.Input); len(picked) > 0 {
			toolSchemas = picked
		}
	}
	inferStart := time.Now()
	var sb strings.Builder
	var inferErr string
	var totalTokens int

	type ExecutedTool struct {
		Name   string `json:"name"`
		Input  any    `json:"input"`
		Output string `json:"output"`
	}
	var executedToolCalls []ExecutedTool

	const maxToolRounds = 10
	for range maxToolRounds {
		var effort types.ReasoningEffort
		switch req.ReasoningEffort {
		case "low":
			effort = types.ReasoningEffortLow
		case "medium":
			effort = types.ReasoningEffortMedium
		case "high":
			effort = types.ReasoningEffortHigh
		default:
			effort = types.ReasoningEffortNone
		}

		inferReq := &types.InferRequest{
			Messages:        history,
			MaxTokens:       4096,
			Temperature:     0.7,
			Tools:           toolSchemas,
			ReasoningEffort: effort,
		}

		// [新增] 读取 SurpriseIndex 决定推理策略
		if agentCtrl != nil {
			si := agentCtrl.SurpriseIndex()
			if si > 0 && si < 0.3 {
				// FastPath: 减少 maxTokens，禁用 chain-of-thought
				inferReq.MaxTokens = 1024
				inferReq.ReasoningEffort = types.ReasoningEffortNone
			}
		}

		// 组装 InferOption，确保 Tools/MaxTokens/Temperature/ReasoningEffort 全部传递
		streamOpts := []types.InferOption{
			types.WithMaxTokens(inferReq.MaxTokens),
			types.WithTemperature(inferReq.Temperature),
		}
		if inferReq.ReasoningEffort != types.ReasoningEffortNone {
			streamOpts = append(streamOpts, types.WithReasoningEffort(inferReq.ReasoningEffort))
		}
		if len(inferReq.Tools) > 0 {
			streamOpts = append(streamOpts, types.WithTools(inferReq.Tools))
		}
		ch, err := p.StreamInfer(ctx, inferReq.Messages, streamOpts...)
		if err != nil {
			if tw != nil {
				tw.WriteError("infer_error", truncate(err.Error(), 300))
			}
			s.writeSSEError(w, flusher, "infer_error", truncate(err.Error(), 300), sessionID, err)
			return
		}

		// 收集本轮 text delta、reasoning delta 和 tool_call 事件
		var roundText strings.Builder
		var roundReasoning strings.Builder
		var toolCalls []map[string]json.RawMessage
		var clientCancelled bool
		ctxDoneCh := ctx.Done()
	roundLoop:
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					break roundLoop
				}
				switch ev.Type {
				case types.StreamThinking:
					roundReasoning.WriteString(ev.Content)
				case types.StreamTextDelta:
					if ev.Content != "" {
						if !clientCancelled {
							writeSSE(w, flusher, "token", map[string]string{"content": ev.Content})
						}
						roundText.WriteString(ev.Content)
						sb.WriteString(ev.Content)
					}
					if t := ev.Usage.InputTokens + ev.Usage.OutputTokens; t > 0 {
						totalTokens = t
					}
				case types.StreamToolCall:
					var call map[string]json.RawMessage
					if json.Unmarshal([]byte(ev.Content), &call) == nil {
						toolCalls = append(toolCalls, call)
					}
				case types.StreamError:
					if inferErr == "" {
						inferErr = ev.Content
					}
				case types.StreamCancelled:
					if t := ev.Usage.InputTokens + ev.Usage.OutputTokens; t > 0 {
						totalTokens = t
					}
					break roundLoop
				}
			case <-ctxDoneCh:
				clientCancelled = true
				ctxDoneCh = nil
			}
		}

		if clientCancelled {
			if totalTokens == 0 {
				if mt, ok := p.Tokenizer().(protocol.MultimodalTokenizer); ok {
					totalTokens = mt.EstimateRequest(inferReq)
				}
			}
			// 如果客户端断开了连接，中断后续工具调用，不再进入下一轮 Tool 循环
			break
		}

		// 没有 tool_call → 推理完成，退出循环
		if len(toolCalls) == 0 || s.ToolProvider == nil {
			break
		}

		// 有 tool_call：构造 assistant 消息（含 tool_use parts），执行工具，加 tool_result
		assistantParts := make([]any, 0, 1+len(toolCalls))
		if roundText.Len() > 0 {
			assistantParts = append(assistantParts, map[string]any{"type": "text", "text": roundText.String()})
		}
		for _, tc := range toolCalls {
			var toolID, toolName string
			var inputRaw json.RawMessage
			if b, ok := tc["id"]; ok {
				json.Unmarshal(b, &toolID) //nolint:errcheck
			}
			if b, ok := tc["name"]; ok {
				json.Unmarshal(b, &toolName) //nolint:errcheck
			}
			if b, ok := tc["input"]; ok {
				inputRaw = b
			}
			assistantParts = append(assistantParts, map[string]any{
				"type":  "tool_use",
				"id":    toolID,
				"name":  toolName,
				"input": inputRaw,
			})
		}
		assistantMsg := types.Message{Role: "assistant", Parts: assistantParts}
		if roundReasoning.Len() > 0 {
			assistantMsg.ReasoningContent = roundReasoning.String()
		}
		history = append(history, assistantMsg)

		// 执行每个工具，收集 tool_result
		toolResultParts := make([]any, 0, len(toolCalls))
		for _, tc := range toolCalls {
			var toolID, toolName string
			var inputRaw json.RawMessage
			if b, ok := tc["id"]; ok {
				json.Unmarshal(b, &toolID) //nolint:errcheck
			}
			if b, ok := tc["name"]; ok {
				json.Unmarshal(b, &toolName) //nolint:errcheck
			}
			if b, ok := tc["input"]; ok {
				inputRaw = b
			}
			writeSSE(w, flusher, "tool_call", map[string]string{"name": toolName})
			result, execErr := s.ToolProvider.ExecuteTool(ctx, toolName, inputRaw)
			var resultText string
			if execErr != nil {
				resultText = "error: " + execErr.Error()
			} else if result != nil {
				resultText = string(result.Output)
			}
			slog.Info("server: tool executed", "name", toolName, "ok", execErr == nil)
			toolResultParts = append(toolResultParts, map[string]any{
				"type":        "tool_result",
				"tool_use_id": toolID,
				// name 字段供 Gemini adapter 的 FunctionResponse 匹配工具名称；
				// Anthropic/OpenAI 适配器不使用此字段（忽略未知 key），不影响兼容性
				"name":    toolName,
				"content": resultText,
			})

			// MCP 工具可能返回图片（type="image" content block）。
			// 将 ImageParts 追加到同一 toolResultParts 切片：
			//   - Anthropic adapter：将 ImagePart 与 tool_result block 一起放入 content 数组
			//   - OpenAI adapter：parseUserParts 提取 ImagePart 为独立 role="user" 视觉消息
			//   - normalizeInferRequest() 自动对图片做降采样/格式转换
			if result != nil && len(result.ImageParts) > 0 {
				slog.Info("server: tool returned images", "name", toolName, "count", len(result.ImageParts))
				for _, img := range result.ImageParts {
					toolResultParts = append(toolResultParts, img)
				}
			}

			var inputObj any
			if len(inputRaw) > 0 {
				json.Unmarshal(inputRaw, &inputObj) //nolint:errcheck
			}
			executedToolCalls = append(executedToolCalls, ExecutedTool{
				Name:   toolName,
				Input:  inputObj,
				Output: resultText,
			})
		}
		history = append(history, types.Message{Role: "user", Parts: toolResultParts})
	}
	inferLatencyMs := time.Since(inferStart).Milliseconds()

	// 推理成功返回但无内容（超时/内容过滤/空响应）
	if sb.Len() == 0 && inferErr == "" {
		inferErr = "推理返回空内容，请检查模型配置或重试"
	}
	if inferErr != "" {
		if tw != nil {
			tw.WriteError("empty_response", inferErr)
		}
		s.writeSSEError(w, flusher, "empty_response", inferErr, sessionID, apperr.New(apperr.CodeInternal, "log event"))
		return
	}

	// ── 持久化 assistant 回复 ─────────────────────────────────────────────
	reply := sb.String()

	saveCtx, saveCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer saveCancel()

	if reply != "" || len(executedToolCalls) > 0 {
		var tcJson string
		if len(executedToolCalls) > 0 {
			b, _ := json.Marshal(executedToolCalls)
			tcJson = string(b)
		}
		if err := s.SaveMessage(saveCtx, sessionID, "assistant", reply, tcJson, inferLatencyMs); err != nil {
			slog.Error("server: saveMessage assistant", "session", sessionID, "err", err)
		}
		if tw != nil {
			tw.WriteTurn("assistant", reply, inferLatencyMs, totalTokens)
		}
	}
	if isFirstTurn {
		_ = s.UpdateSessionTitle(saveCtx, sessionID, req.Input)
	}
	_ = s.TouchSession(saveCtx, sessionID)

	slog.Info("server: turn complete",
		"session", sessionID,
		"latency_ms", inferLatencyMs,
		"tokens", totalTokens,
		"reply_bytes", len(reply),
		"client_cancelled", ctx.Err() != nil,
	)

	// message.after hook：fire-and-forget，不阻塞响应
	s.Hooks.Fire("message.after", map[string]string{
		"POLARIS_REPLY":      reply,
		"POLARIS_SESSION_ID": sessionID,
		"POLARIS_CHANNEL":    "web",
	})

	// [新增] 推理完成后通知 FSM（非阻塞）
	if agentCtrl != nil {
		go func() { _ = agentCtrl.SendIntent(types.TriggerExecuteDone) }()
	}

	writeSSE(w, flusher, "complete", map[string]any{
		"session_id":  sessionID,
		"duration_ms": inferLatencyMs,
	})
}

func (s *ChatHandler) InjectSystemPrompt(ctx context.Context, agentCtrl protocol.AgentController, history []types.Message, userQuery string) []types.Message { //nolint:gocyclo,nestif
	if agentCtrl == nil || agentCtrl.Memory() == nil {
		return history
	}

	ic, ok := agentCtrl.Memory().Working().Immutable().(*store.ImmutableCore)
	if !ok {
		return history
	}

	// ── stable 层：身份 / 用户自定义指令 / 模型引导 / 平台提示 ────────────

	// 用户身份（三层优先级已在 LoadSoulMD 中处理，此处注入结果）
	ic.SoulMDContent = (*s.SoulMDContent)

	// 用户自定义追加指令（~/.polarisagi/polaris/config/prompts/custom_instructions.md）
	ic.CustomInstructions = s.PromptMgr.ReadPrompt("custom_instructions.md", "")

	// M9 激活的系统提示词优先覆盖（general taskType）
	// 三层组装时 SystemPromptTemplate 非空则全量走模板渲染，跳过 stable 层组装
	s.ActivatedSystemPromptMu.RLock()
	activatedPrompt := s.ActivatedSystemPrompt
	s.ActivatedSystemPromptMu.RUnlock()
	// 每轮重置为基础模板，防止 ambient 内容跨请求累积。
	// M9 激活提示词（activatedPrompt != ""）优先覆盖基础模板。
	if activatedPrompt != "" {
		ic.SystemPromptTemplate = activatedPrompt
	} else {
		ic.SystemPromptTemplate = s.BaseSystemPromptTpl
	}

	// 当前 Provider ModelID → 模型感知工具调用引导
	modelID := ""
	if p := s.Registry.PickProvider("default"); p != nil {
		modelID = p.ModelID()
	} else if p := s.Registry.PickProvider("general"); p != nil {
		modelID = p.ModelID()
	}
	ic.ModelID = modelID

	// 模型感知工具调用引导：模板模式（{{.ModelGuidance}}）和三层模式均需注入，移除旧的 "" 守卫。
	if prompt.NeedsToolUseEnforcement(modelID) {
		ic.ModelGuidance = s.PromptMgr.ModelSpecificGuidance(modelID)
		if ic.ModelGuidance == "" {
			// 通用工具调用强制引导（兜底）
			ic.ModelGuidance = "有工具可用时必须立即调用，禁止仅输出执行计划或说明性描述。"
		}
	} else {
		ic.ModelGuidance = ""
	}

	ic.OperationalDirectives = loadOperationalDirectives(s.PromptMgr)

	// 平台感知提示
	ic.PlatformHint = s.PromptMgr.PlatformHintFor(s.ServerPlatform)

	// volatile 层：当前日期（精确到天，不破坏 prefix cache），会话信息由调用方追加
	ic.VolatileBlock = "当前日期：" + time.Now().Format("2006-01-02")

	// Built-in tools — 仅注入工具名列表；描述已由 function schema 传递，避免系统提示词冗余膨胀。
	if s.ToolReg != nil {
		var names []string
		for _, t := range s.ToolReg.List() {
			names = append(names, t.Name)
		}
		if len(names) > 0 {
			ic.BuiltinTools = fmt.Sprintf("%d: %s", len(names), strings.Join(names, ", "))
		}
	}

	// 扩展感知（插件 / MCP / App）— 仅名称 + 连接状态摘要，细节由 BuildToolSchemas() 注入 function schema。
	ic.InstalledPlugins = s.buildExtensionSummary(ctx)

	// Ambient skills 写入独立字段，不拼接进 SystemPromptTemplate。
	// 原因：skill instructions 可能含 {{ }} 语法（代码示例/Jinja/Handlebars），
	// 若拼入模板字符串会导致 template.Parse() 崩溃，系统提示词退化为报错文本。
	// PrependToMessages 在模板渲染完成后再追加 AmbientContext，彻底脱离模板解析器。
	if s.DB != nil {
		ic.AmbientContext = s.buildAmbientSkillsSection(ctx, userQuery)
	}

	return ic.PrependToMessages(history)
}

const (
	maxFullTextChars   = 128_000 // 全文注入总预算（128K字符 ≈ 32K tokens）
	relevanceThreshold = 0.05    // 关键词词元重叠阈值（5%）
)

func relevanceScore(query string, name string, desc string, inst string) float64 {
	queryLower := strings.ToLower(query)
	targetText := strings.ToLower(name + " " + desc + " " + inst)

	queryTokens := strings.Fields(queryLower)
	if len(queryTokens) == 0 {
		return 0
	}

	matchCount := 0
	for _, tk := range queryTokens {
		if strings.Contains(targetText, tk) {
			matchCount++
		}
	}

	return float64(matchCount) / float64(len(queryTokens))
}

// skillTextKey 返回技能文本的缓存 key（sha256 hex）。
func skillTextKey(name, desc, inst string) string {
	h := sha256.Sum256([]byte(name + "\x00" + desc + "\x00" + inst))
	return fmt.Sprintf("%x", h)
}

// cachedSkillEmbed 从缓存读取或调用 Embedder 获取技能向量。
// 失败时返回 nil（调用方降级 Tier 1）。
func cachedSkillEmbed(e search.Embedder, name, desc, inst string) []float32 {
	key := skillTextKey(name, desc, inst)
	skillEmbedCacheMu.RLock()
	if v, ok := skillEmbedCache[key]; ok {
		skillEmbedCacheMu.RUnlock()
		return v
	}
	skillEmbedCacheMu.RUnlock()

	text := name + " " + desc + " " + inst
	v := e.Embed(text)
	if v != nil {
		skillEmbedCacheMu.Lock()
		// 超限时随机淘汰一条（技能数量有界，随机淘汰比 LRU 实现简单且效果相近）
		if len(skillEmbedCache) >= skillEmbedCacheMax {
			for k := range skillEmbedCache {
				delete(skillEmbedCache, k)
				break
			}
		}
		skillEmbedCache[key] = v
		skillEmbedCacheMu.Unlock()
	}
	return v
}

// isSkillRelevant 判断技能是否与用户查询相关。
// Tier 2（Embedder 可用）：余弦相似度 >= EmbedThreshold。
// Tier 1（降级）：词元重叠度 >= relevanceThreshold。
// 任何错误静默降级 Tier 1，不中断聊天主流程。
func (s *ChatHandler) isSkillRelevant(queryVec []float32, query, name, desc, inst string) bool {
	if s.Embedder == nil || queryVec == nil {
		return relevanceScore(query, name, desc, inst) >= relevanceThreshold
	}

	skillVec := cachedSkillEmbed(s.Embedder, name, desc, inst)
	if skillVec == nil {
		return relevanceScore(query, name, desc, inst) >= relevanceThreshold
	}

	threshold := s.EmbedThreshold
	if threshold == 0 {
		threshold = 0.60
	}
	return ffi.VecCosineF32(queryVec, skillVec) >= float32(threshold)
}

// buildAmbientSkillsSection 按 trust_tier 和 ambient_priority 注入 ambient skill instructions
func (s *ChatHandler) buildAmbientSkillsSection(ctx context.Context, userQuery string) string {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT name, description, instructions, plugin_id, ambient_priority, trust_tier
         FROM skills
         WHERE exec_mode='ambient' AND deprecated=0
         ORDER BY trust_tier DESC,
                  CASE ambient_priority WHEN 'always' THEN 0 WHEN 'auto' THEN 1 ELSE 2 END ASC`)
	if err != nil {
		return ""
	}
	defer rows.Close()

	var indexLines []string
	var fullTextParts []string
	fullTextBudget := maxFullTextChars

	var queryVec []float32
	if s.Embedder != nil {
		queryVec = s.Embedder.Embed(userQuery)
	}

	for rows.Next() {
		var name, desc, inst, pluginID, ambientPriority string
		var trustTier int
		if rows.Scan(&name, &desc, &inst, &pluginID, &ambientPriority, &trustTier) != nil {
			continue
		}

		mcpMark := ""
		if s.MCPMgr != nil && s.MCPMgr.IsPluginConnected(pluginID) {
			mcpMark = " [MCP: ✓]"
		} else if pluginID != "" {
			mcpMark = " [MCP: ✗]"
		}

		indexLine := "- " + name + ": " + desc + mcpMark
		indexLines = append(indexLines, indexLine)

		if ambientPriority == "index_only" {
			continue
		}

		if ambientPriority == "auto" {
			if !s.isSkillRelevant(queryVec, userQuery, name, desc, inst) {
				continue
			}
		}

		if fullTextBudget-len(inst) < 0 {
			slog.Warn("ambient skill budget exhausted, index-only fallback", "skill", name)
			continue
		}

		entry := "### " + name + "\n" + inst
		fullTextParts = append(fullTextParts, entry)
		fullTextBudget -= len(entry)
	}

	if len(indexLines) == 0 {
		return ""
	}

	res := "\n\n## Installed Skills\n" + strings.Join(indexLines, "\n")
	if len(fullTextParts) > 0 {
		res += "\n\n## Active Skill Context\n" + strings.Join(fullTextParts, "\n\n")
	}
	return res
}

// SetActivatedSystemPrompt 热更新 M9 激活的系统提示词（goroutine-safe）。
// 由 PromptVersionStore.OnActivate 回调触发，对 task_type='general' 的激活版本生效。
func (s *ChatHandler) SetActivatedSystemPrompt(taskType, promptText string) {
	if taskType != "general" {
		return
	}
	s.ActivatedSystemPromptMu.Lock()
	s.ActivatedSystemPrompt = promptText
	s.ActivatedSystemPromptMu.Unlock()
}

// buildExtensionSummary 构建插件/MCP/App 感知摘要字符串（单行，| 分隔）。
// 只注入名称和连接状态；详细工具参数由 BuildToolSchemas() 注入 function schema 传递，避免双重注入。
func (s *ChatHandler) buildExtensionSummary(ctx context.Context) string {
	var parts []string
	if s.DB != nil {
		if plugParts := s.queryPluginSummary(ctx); len(plugParts) > 0 {
			parts = append(parts, "Plugins: "+strings.Join(plugParts, ", "))
		}
		if appParts := s.queryAppSummary(ctx); len(appParts) > 0 {
			parts = append(parts, "Apps: "+strings.Join(appParts, ", "))
		}
	}
	if s.MCPMgr != nil {
		if mcpParts := s.standaloneMCPSummary(); len(mcpParts) > 0 {
			parts = append(parts, "MCPs: "+strings.Join(mcpParts, ", "))
		}
	}
	return strings.Join(parts, " | ")
}

// queryPluginSummary 查询已安装插件名称与 MCP 整体连接状态（格式："PluginName(✓)"）。
// ✓ = 所有 MCP 已连接；~ = 部分连接；✗ = 未连接。
func (s *ChatHandler) queryPluginSummary(ctx context.Context) []string {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id, name, display_name, mcp_policy FROM plugins WHERE enabled=1`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	connectedSet := make(map[string]bool)
	if s.MCPMgr != nil {
		for _, srv := range s.MCPMgr.ListServers() {
			connectedSet[srv.ID] = srv.Connected
		}
	}

	var result []string
	for rows.Next() {
		var plugID, plugName, displayName, policyJSON string
		if rows.Scan(&plugID, &plugName, &displayName, &policyJSON) != nil {
			continue
		}
		label := displayName
		if label == "" {
			label = plugName
		}

		var policy map[string]map[string]any
		_ = json.Unmarshal([]byte(policyJSON), &policy)

		connected, total := 0, 0
		for serverName, entry := range policy {
			enabled := true
			if v, ok := entry["enabled"].(bool); ok {
				enabled = v
			}
			if !enabled {
				continue
			}
			total++
			if connectedSet["plugin_"+plugID+"_"+serverName] {
				connected++
			}
		}

		mark := "✗"
		if total > 0 && connected == total {
			mark = "✓"
		} else if connected > 0 {
			mark = "~"
		}
		result = append(result, label+"("+mark+")")
	}
	return result
}

// queryAppSummary 查询已启用 App 的显示名称列表。
func (s *ChatHandler) queryAppSummary(ctx context.Context) []string {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT display_name, name FROM apps WHERE enabled=1`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var result []string
	for rows.Next() {
		var displayName, name string
		if rows.Scan(&displayName, &name) != nil {
			continue
		}
		label := displayName
		if label == "" {
			label = name
		}
		result = append(result, label)
	}
	return result
}

// standaloneMCPSummary 返回非插件独立 MCP 服务的名称+连接状态列表。
func (s *ChatHandler) standaloneMCPSummary() []string {
	result := make([]string, 0, len(s.MCPMgr.ListServers()))
	for _, srv := range s.MCPMgr.ListServers() {
		if strings.HasPrefix(srv.ID, "plugin_") {
			continue
		}
		mark := "✗"
		if srv.Connected {
			mark = "✓"
		}
		result = append(result, srv.Name+" "+mark)
	}
	return result
}

func loadOperationalDirectives(pm *prompt.Manager) string {
	var opDirectives []string

	if op := pm.ReadPrompt("operational/tool_use.md", ""); op != "" {
		opDirectives = append(opDirectives, op)
	}
	if op := pm.ReadPrompt("operational/task_completion.md", ""); op != "" {
		opDirectives = append(opDirectives, op)
	}
	if op := pm.ReadPrompt("operational/execution_discipline.md", ""); op != "" {
		opDirectives = append(opDirectives, op)
	}
	if op := pm.ReadPrompt("operational/memory_hygiene.md", ""); op != "" {
		opDirectives = append(opDirectives, op)
	}
	if op := pm.ReadPrompt("operational/coding_style.md", ""); op != "" {
		opDirectives = append(opDirectives, op)
	}
	if op := pm.ReadPrompt("operational/output_efficiency.md", ""); op != "" {
		opDirectives = append(opDirectives, op)
	}
	if op := pm.ReadPrompt("operational/risky_actions.md", ""); op != "" {
		opDirectives = append(opDirectives, op)
	}

	if len(opDirectives) > 0 {
		return strings.Join(opDirectives, "\n\n")
	}
	return ""
}
