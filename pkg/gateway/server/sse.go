package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/cognition/memory"
)

func writeSSE(w http.ResponseWriter, flusher http.Flusher, eventType string, payload any) {
	data, _ := json.Marshal(payload)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, data)
	flusher.Flush()
}

func (s *Server) writeSSEError(w http.ResponseWriter, flusher http.Flusher, code, message string, sessionID string, err error) {
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

func (s *Server) handleAgentStream(w http.ResponseWriter, r *http.Request) { //nolint:gocyclo
	var req struct {
		Input       string          `json:"input"`
		SessionID   string          `json:"session_id,omitempty"`
		RunID       string          `json:"run_id,omitempty"`
		Attachments []sseAttachment `json:"attachments,omitempty"`
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
	if err := s.ensureSession(ctx, sessionID); err != nil {
		s.writeSSEError(w, flusher, "session_error", err.Error(), sessionID, err)
		return
	}
	// session.new hook：用户发起新会话时触发（req.SessionID 为空意味着 /new 后首条消息）
	if isNewSession {
		s.hooks.Fire("session.new", map[string]string{
			"POLARIS_SESSION_ID": sessionID,
			"POLARIS_CHANNEL":    "web",
		})
	}

	// message.before hook：同步拦截，非零退出 = 拒绝本条消息
	if blocked, reason := s.hooks.FireBefore("message.before", map[string]string{
		"POLARIS_MESSAGE":    req.Input,
		"POLARIS_SESSION_ID": sessionID,
		"POLARIS_CHANNEL":    "web",
	}); blocked {
		s.writeSSEError(w, flusher, "hook_blocked", reason, sessionID, nil)
		return
	}

	// 加载历史消息（多轮上下文）
	history, err := s.loadMessages(ctx, sessionID)
	if err != nil {
		s.writeSSEError(w, flusher, "history_error", err.Error(), sessionID, err)
		return
	}
	isFirstTurn := len(history) == 0

	// 注入 System Prompt
	history = s.injectSystemPrompt(history)

	// ── Transcript ────────────────────────────────────────────────────────
	// 非阻塞：打开失败只警告，不中断对话。
	tw, twErr := openTranscript(s.transcriptDir, sessionID, isFirstTurn)
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
			userPromptBuilder.WriteString(fmt.Sprintf("\n\n[System: 用户挂载了系统附件 %s", att.URI))
			if att.Name != "" {
				userPromptBuilder.WriteString(fmt.Sprintf(" (原始文件名: %s)", att.Name))
			}
			userPromptBuilder.WriteString("]")
			continue
		}

		// 必须是 workspace:// 协议才能读本地文件
		if !strings.HasPrefix(att.URI, "workspace://") {
			slog.Warn("server: non-workspace URI skipped for media attachment", "uri", att.URI)
			continue
		}

		localPath := filepath.Join(s.dataDir, "workspace", strings.TrimPrefix(att.URI, "workspace://"))

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
				userPromptBuilder.WriteString(fmt.Sprintf(
					"\n\n[System: 视频文件 %s (%.1fMB) 超过内联上限（20MB），未能传递给模型。请使用较小的视频片段。]",
					name, float64(fi.Size())/(1024*1024),
				))
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
			mediaParts = append(mediaParts, protocol.ImagePart{
				Type:      "image",
				MediaType: att.MimeType,
				Data:      raw,
			})
		} else {
			// video/* → Gemini inlineData 方式（已通过上方大小门控，≤20MB）
			mediaParts = append(mediaParts, protocol.VideoPart{
				Type:      "video",
				MediaType: att.MimeType,
				Data:      raw,
			})
		}
	}

	finalInput := strings.TrimSpace(userPromptBuilder.String())
	userMsg := protocol.Message{Role: "user", Content: finalInput}

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
			mediaParts = append(mediaParts, protocol.ImagePart{
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
	if err := s.saveMessage(ctx, sessionID, "user", finalInput, "", 0); err != nil {
		slog.Error("server: saveMessage user", "session", sessionID, "err", err)
	}
	if tw != nil {
		tw.WriteTurn("user", req.Input, 0, 0)
	}

	// ── 选取最优 Provider ─────────────────────────────────────────────────
	// 优先用 "default" 角色（对话模型），次选 "general"（参与全局 LB）
	p := s.registry.PickProvider("default")
	if p == nil {
		p = s.registry.PickProvider("general")
	}
	if p == nil {
		if tw != nil {
			tw.WriteError("no_provider", "未配置任何启用的 LLM 厂商")
		}
		s.writeSSEError(w, flusher, "no_provider", "未配置任何启用的 LLM 厂商，请在「模型」页添加并启用厂商", sessionID, nil)
		return
	}

	// 上下文压缩：provider 已就绪，history 包含本轮新用户消息，超阈值则压缩
	if s.compressor.NeedsCompact(history) {
		writeSSE(w, flusher, "status", map[string]any{"type": "compacting", "message": "正在压缩上下文..."})
		if compacted, res, err := s.compressor.Compact(ctx, sessionID, history, p); err == nil && !res.Skipped {
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

	toolSchemas := s.buildToolSchemas()
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
		inferReq := &protocol.InferRequest{
			Messages:    history,
			MaxTokens:   4096,
			Temperature: 0.7,
			Tools:       toolSchemas,
		}
		ch, err := p.StreamInfer(ctx, inferReq)
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
	roundLoop:
		for {
			select {
			case ev, ok := <-ch:
				if !ok {
					break roundLoop
				}
				switch ev.Type {
				case protocol.StreamThinking:
					roundReasoning.WriteString(ev.Content)
				case protocol.StreamTextDelta:
					if ev.Content != "" {
						writeSSE(w, flusher, "token", map[string]string{"content": ev.Content})
						roundText.WriteString(ev.Content)
						sb.WriteString(ev.Content)
					}
					if t := ev.Usage.InputTokens + ev.Usage.OutputTokens; t > 0 {
						totalTokens = t
					}
				case protocol.StreamToolCall:
					var call map[string]json.RawMessage
					if json.Unmarshal([]byte(ev.Content), &call) == nil {
						toolCalls = append(toolCalls, call)
					}
				case protocol.StreamError:
					if inferErr == "" {
						inferErr = ev.Content
					}
				}
			case <-ctx.Done():
				return
			}
		}

		// 没有 tool_call → 推理完成，退出循环
		if len(toolCalls) == 0 || s.toolExec == nil {
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
		assistantMsg := protocol.Message{Role: "assistant", Parts: assistantParts}
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
			result, execErr := s.toolExec(ctx, toolName, inputRaw)
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
		history = append(history, protocol.Message{Role: "user", Parts: toolResultParts})
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
		s.writeSSEError(w, flusher, "empty_response", inferErr, sessionID, perrors.New(perrors.CodeInternal, "log event"))
		return
	}

	// ── 持久化 assistant 回复 ─────────────────────────────────────────────
	reply := sb.String()
	if reply != "" || len(executedToolCalls) > 0 {
		var tcJson string
		if len(executedToolCalls) > 0 {
			b, _ := json.Marshal(executedToolCalls)
			tcJson = string(b)
		}
		if err := s.saveMessage(ctx, sessionID, "assistant", reply, tcJson, inferLatencyMs); err != nil {
			slog.Error("server: saveMessage assistant", "session", sessionID, "err", err)
		}
		if tw != nil {
			tw.WriteTurn("assistant", reply, inferLatencyMs, totalTokens)
		}
	}
	if isFirstTurn {
		_ = s.updateSessionTitle(ctx, sessionID, req.Input)
	}
	s.touchSession(ctx, sessionID)

	slog.Info("server: turn complete",
		"session", sessionID,
		"latency_ms", inferLatencyMs,
		"tokens", totalTokens,
		"reply_bytes", len(reply),
	)

	// message.after hook：fire-and-forget，不阻塞响应
	s.hooks.Fire("message.after", map[string]string{
		"POLARIS_REPLY":      reply,
		"POLARIS_SESSION_ID": sessionID,
		"POLARIS_CHANNEL":    "web",
	})

	writeSSE(w, flusher, "complete", map[string]any{
		"session_id":  sessionID,
		"duration_ms": inferLatencyMs,
	})
}

func (s *Server) injectSystemPrompt(history []protocol.Message) []protocol.Message { //nolint:gocyclo,nestif
	if s.agent == nil || s.agent.Memory() == nil {
		return history
	}

	ic, ok := s.agent.Memory().Working().Immutable().(*memory.ImmutableCore)
	if !ok {
		return history
	}

	// ── stable 层：身份 / 用户自定义指令 / 模型引导 / 平台提示 ────────────

	// 用户身份（三层优先级已在 LoadSoulMD 中处理，此处注入结果）
	ic.SoulMDContent = s.soulMDContent

	// 用户自定义追加指令（~/.polarisagi/polaris/config/prompts/custom_instructions.md）
	ic.CustomInstructions = memory.ReadPrompt("custom_instructions.md", "")

	// M9 激活的系统提示词优先覆盖（general taskType）
	// 三层组装时 SystemPromptTemplate 非空则全量走模板渲染，跳过 stable 层组装
	s.activatedSystemPromptMu.RLock()
	activatedPrompt := s.activatedSystemPrompt
	s.activatedSystemPromptMu.RUnlock()
	if activatedPrompt != "" && ic.SystemPromptTemplate == "" {
		ic.SystemPromptTemplate = activatedPrompt
	}

	// 当前 Provider ModelID → 模型感知工具调用引导
	modelID := ""
	if p := s.registry.PickProvider("default"); p != nil {
		modelID = p.ModelID()
	} else if p := s.registry.PickProvider("general"); p != nil {
		modelID = p.ModelID()
	}
	ic.ModelID = modelID

	// 三层组装时才注入模型专属引导（模板模式由模板自行处理）
	if ic.SystemPromptTemplate == "" {
		if memory.NeedsToolUseEnforcement(modelID) {
			ic.ModelGuidance = memory.ModelSpecificGuidance(modelID)
			if ic.ModelGuidance == "" {
				// 通用工具调用强制引导（兜底）
				ic.ModelGuidance = "有工具可用时必须立即调用，禁止仅输出执行计划或说明性描述。"
			}
		} else {
			ic.ModelGuidance = ""
		}

		// 平台感知提示
		ic.PlatformHint = memory.PlatformHintFor(s.serverPlatform)

		// volatile 层：当前日期（精确到天，不破坏 prefix cache），会话信息由调用方追加
		ic.VolatileBlock = "当前日期：" + time.Now().Format("2006-01-02")
	}

	// Built-in tools
	if s.toolReg != nil {
		var builtin []string
		for _, t := range s.toolReg.List() {
			builtin = append(builtin, t.Name+" - "+t.Description)
		}
		ic.BuiltinTools = strings.Join(builtin, "\n")
	}

	// 插件 + 独立 MCP 感知注入（让 LLM 了解插件维度，区别于工具 schema 层）
	if s.db != nil || s.mcpMgr != nil { //nolint:nestif
		var pluginLines []string

		// 1. 已安装插件（来自 plugins 表），格式：display_name: description [MCPs: name ✓/✗]
		if s.db != nil {
			plugRows, err := s.db.Query(
				`SELECT id, name, display_name, description, mcp_policy FROM plugins WHERE enabled=1`)
			if err == nil {
				defer plugRows.Close()

				// 预取 MCP 连接状态
				connectedSet := make(map[string]bool)
				if s.mcpMgr != nil {
					for _, srv := range s.mcpMgr.ListServers() {
						connectedSet[srv.ID] = srv.Connected
					}
				}

				for plugRows.Next() {
					var plugID, plugName, displayName, desc, policyJSON string
					if plugRows.Scan(&plugID, &plugName, &displayName, &desc, &policyJSON) != nil {
						continue
					}
					label := displayName
					if label == "" {
						label = plugName
					}

					var policy map[string]map[string]any
					_ = json.Unmarshal([]byte(policyJSON), &policy)

					var mcpParts []string
					for serverName, entry := range policy {
						enabled := true
						if v, ok := entry["enabled"].(bool); ok {
							enabled = v
						}
						if !enabled {
							continue
						}
						serverID := "plugin_" + plugID + "_" + serverName
						scopedName := plugName + "-" + serverName
						mark := "✗"
						if connectedSet[serverID] {
							mark = "✓"
						}
						mcpParts = append(mcpParts, scopedName+" "+mark)
					}

					line := "- " + label + ": " + desc
					if len(mcpParts) > 0 {
						line += " [MCPs: " + strings.Join(mcpParts, ", ") + "]"
					}
					pluginLines = append(pluginLines, line)
				}
			}
		}

		// 2. 独立 MCP（mcp_servers 表，ID 无 plugin_ 前缀）
		if s.mcpMgr != nil {
			var standaloneMCPs []string
			for _, srv := range s.mcpMgr.ListServers() {
				if strings.HasPrefix(srv.ID, "plugin_") {
					continue
				}
				mark := "✗"
				if srv.Connected {
					mark = "✓"
				}
				standaloneMCPs = append(standaloneMCPs, srv.Name+" "+mark)
			}
			if len(standaloneMCPs) > 0 {
				pluginLines = append(pluginLines, "Standalone MCPs: "+strings.Join(standaloneMCPs, ", "))
			}
		}

		ic.InstalledPlugins = strings.Join(pluginLines, "\n")
	}

	// Ambient skills（追加到 stable 层）——唯一来源是 skills 表，State-in-DB 原则。
	// 插件的 ambient skill 在安装时已由 registerPluginSkills 写入 skills 表，此处统一查询。
	if s.db != nil {
		ic.SystemPromptTemplate += s.buildAmbientSkillsSection()
	}

	return ic.PrependToMessages(history)
}

// buildAmbientSkillsSection 查询所有 ambient skill 的 instructions，返回可追加到 system prompt 的段落。
// 独立函数消除 injectSystemPrompt 的 nestif 深度。
func (s *Server) buildAmbientSkillsSection() string {
	rows, err := s.db.Query(`SELECT name, instructions FROM skills WHERE exec_mode='ambient' AND deprecated=0`)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var name, inst string
		if rows.Scan(&name, &inst) == nil {
			parts = append(parts, "Ambient Skill: "+name+"\n"+inst)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "\n\n" + strings.Join(parts, "\n\n")
}

// SetActivatedSystemPrompt 热更新 M9 激活的系统提示词（goroutine-safe）。
// 由 PromptVersionStore.OnActivate 回调触发，对 task_type='general' 的激活版本生效。
func (s *Server) SetActivatedSystemPrompt(taskType, promptText string) {
	if taskType != "general" {
		return
	}
	s.activatedSystemPromptMu.Lock()
	s.activatedSystemPrompt = promptText
	s.activatedSystemPromptMu.Unlock()
}
