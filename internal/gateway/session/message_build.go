package session

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/pkg/types"
)

// maxVideoInlineBytes Gemini inlineData 视频大小上限（20MB）。超过此值应走
// Gemini File API 上传后使用 URI，当前不支持，拒绝处理。原
// chat/sse_media.go 常量原样迁入。
const maxVideoInlineBytes = 20 * 1024 * 1024

// buildUserMessage 从 Request（文本 + VFS 附件 + 已解码的图片 Parts）构造本轮
// 用户消息与拼接后的 finalInput。
//
// 原 chat/sse_stream_helpers.go buildStreamUserMessage 逐行迁入（行为等价），
// legacy base64 解码职责已上移至 HTTP 边界层——Request.ImageParts 到达本函数
// 前已是解码后的 []types.ImagePart（见 types.go Request 字段注释），本函数
// 内不再做 base64.StdEncoding.DecodeString。
//
//nolint:gocyclo // 原属 HandleAgentStream 整体 nolint:gocyclo 覆盖范围内的既有复杂度，迁移未新增分支
func (o *orchestrator) buildUserMessage(req Request) (finalInput string, userMsg types.Message) {
	var userPromptBuilder strings.Builder
	userPromptBuilder.WriteString(req.Input)

	var hasMedia bool
	mediaParts := make([]any, 0, len(req.Attachments)+len(req.ImageParts))

	// 处理 VFS 附件
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
			slog.Warn("session: non-workspace URI skipped for media attachment", "uri", att.URI)
			continue
		}

		localPath := filepath.Join(o.dataDir, "workspace", strings.TrimPrefix(att.URI, "workspace://"))

		if isVideo {
			// 视频大小门控：超过 Gemini inlineData 上限（20MB）直接拒绝，避免 OOM
			fi, statErr := os.Stat(localPath)
			if statErr != nil {
				slog.Warn("session: failed to stat video attachment", "uri", att.URI, "err", statErr)
				continue
			}
			if fi.Size() > maxVideoInlineBytes {
				slog.Warn("session: video too large for inline, skipping", "uri", att.URI, "size", fi.Size())
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
			slog.Warn("session: failed to read media attachment", "uri", att.URI, "err", err)
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

	// 兼容老版本的 Base64 图片：HTTP 边界层已完成 base64 解码，这里只需原样并入。
	if len(req.ImageParts) > 0 {
		hasMedia = true
		for _, ip := range req.ImageParts {
			mediaParts = append(mediaParts, ip)
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
