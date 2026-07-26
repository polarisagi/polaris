package chat

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// agentStreamRequest 是 HandleAgentStream 的请求体。
type agentStreamRequest struct {
	Input           string          `json:"input"`
	SessionID       string          `json:"session_id,omitempty"`
	RunID           string          `json:"run_id,omitempty"`
	ModelID         string          `json:"model_id,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Attachments     []sseAttachment `json:"attachments,omitempty"`
	// back-compat
	ImageParts []sseImagePart `json:"image_parts,omitempty"`
}

// buildStreamUserMessage 从请求体（文本 + VFS 附件 + 兼容旧版 Base64 图片）构造
// 本轮用户消息与拼接后的 finalInput。从 HandleAgentStream 中抽出（原函数体逐行
// 迁移，行为完全等价），仅用于满足 R7 文件行数上限。
//
//nolint:gocyclo // 原属 HandleAgentStream 整体 nolint:gocyclo 覆盖范围内的既有复杂度，迁移未新增分支
func (s *ChatHandler) buildStreamUserMessage(req agentStreamRequest) (finalInput string, userMsg types.Message) {
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

	finalInput = strings.TrimSpace(userPromptBuilder.String())
	userMsg = types.Message{Role: "user", Content: finalInput}

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

	return finalInput, userMsg
}

// acquireStreamAgent 从 AgentPool 获取本次流式对话的 AgentController，处理资源耗尽降级
// （从 HandleAgentStream 拆出，nestif 治理，行为不变）。ok=false 表示已写入 SSE 错误/降级
// 提示，调用方应立即 return；release 仅在 ok=true 时有效，调用方需 defer release()。
func (s *ChatHandler) acquireStreamAgent(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, req agentStreamRequest, sessionID string) (protocol.AgentController, func(), bool) {
	agentCtrl, release, err := s.AgentPool.Acquire(ctx, sessionID)
	if err == nil {
		return agentCtrl, release, true
	}

	var aerr *apperr.Error
	if errors.As(err, &aerr) && aerr.Code == apperr.CodeResourceExhausted {
		// 后台计算请求
		if req.RunID != "" || req.ReasoningEffort == "background" {
			s.WriteSSEError(w, flusher, "system_notice", "后台提炼排队中", sessionID, nil)
			return nil, nil, false
		}
		// 前台对话请求
		WriteSSE(w, flusher, "system_notice", map[string]any{
			"message": "系统当前负载较高，已为您转入沙箱保护模式，稍等片刻",
			"retry":   true,
		})
		return nil, nil, false
	}
	s.WriteSSEError(w, flusher, "agent_pool_error", "failed to acquire agent: "+err.Error(), sessionID, err)
	return nil, nil, false
}

// handleStreamFSMEvent 处理单条 FSM 流事件：按类型转发 SSE 并累积 reply/inferErr
// （从 handleAgentStreamFSM 拆出，gocyclo 治理，行为不变）。
// 返回 stop=true 时调用方应结束事件循环（task_done 状态事件）。
func (s *ChatHandler) handleStreamFSMEvent(
	w http.ResponseWriter,
	flusher http.Flusher,
	sessionID string,
	ev types.AgentStreamEvent,
	systemPromptGuard *guard.SystemPromptGuard,
	reply *strings.Builder,
	inferErr *string,
) (stop bool) {
	switch ev.Type {
	case types.AgentStreamEventThinking:
		WriteSSE(w, flusher, "reasoning", map[string]any{"content": ev.Content})
	case types.AgentStreamEventToken:
		cleaned, err := systemPromptGuard.Scan(ev.Content, true)
		if err != nil {
			slog.Warn("server: system prompt leak detected", "session_id", sessionID, "err", err)
		}
		ev.Content = cleaned
		WriteSSE(w, flusher, "token", map[string]any{"content": ev.Content})
		reply.WriteString(ev.Content)
	case types.AgentStreamEventToolCall:
		msg := fmt.Sprintf("Executing tool %s...", ev.ToolName)
		WriteSSE(w, flusher, "status", map[string]any{"type": "tool_call", "message": msg})
	case types.AgentStreamEventToolResult:
		WriteSSE(w, flusher, "status", map[string]any{"type": "tool_result", "message": ev.Content})
	case types.AgentStreamEventError:
		if *inferErr == "" {
			*inferErr = ev.Content
		}
		s.WriteSSEError(w, flusher, "fsm_error", ev.Content, sessionID, nil)
	case types.AgentStreamEventStatus:
		if ev.Content == "task_done" {
			return true
		}
		WriteSSE(w, flusher, "status", map[string]any{"type": "info", "message": ev.Content})
	}
	return false
}

// ExecutedTool 记录一次工具调用的名称/输入/输出，用于持久化到消息的 tool_calls 字段。
