package chat

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

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

// ExecutedTool 记录一次工具调用的名称/输入/输出，用于持久化到消息的 tool_calls 字段。
