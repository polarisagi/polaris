package llm

// media_opt.go — 推理层多模态内容预处理
// 职责：在将请求发给任何 Provider 前，统一对图片/视频内容做尺寸/格式标准化。
// 位置选择：L0 推理层（不是 L3 网关层），使得所有调用方——
//   - pkg/gateway/server（HTTP 网关上传）
//   - pkg/cognition/kernel（Agent FSM 工具调用结果）
//   - pkg/extensions（MCP/Plugin/Browser 插件返回图片）
//   - pkg/swarm（多 Agent 编排中间推理）
// ——都能自动获益，无需各自实现。
//
// 仅使用 Go 标准库（image/jpeg、image/png、image/gif、image/draw），零外部依赖。
//
// 设计约束（Tier-0）：
//   - 图片最长边 > 1568px 时等比降采样（Anthropic/Gemini 推荐的最保守上限）
//   - 非 JPEG 可解码格式（PNG/GIF）转 JPEG quality=85，减少 Base64 传输体积
//   - WebP / BMP 等标准库不支持解码，原样透传（provider 自行处理）
//   - 处理是幂等的：已符合条件的 JPEG 不做二次编码

import (
	"bytes"
	"image"
	"image/draw"
	_ "image/gif" // 注册 GIF 解码器（side-effect import）
	"image/jpeg"
	_ "image/jpeg" // 注册 JPEG 解码器（side-effect import）
	_ "image/png"  // 注册 PNG 解码器（side-effect import）
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/pkg/types"
)

const (
	// mediaOptMaxImageSide 图片最长边上限（像素）。
	// 取三家主流 provider 最保守限制（Anthropic Claude 1568px）。
	// 超过此值的额外像素不提升 LLM 视觉理解能力，但线性增加 token 用量和传输延迟。
	mediaOptMaxImageSide = 1568

	// mediaOptJPEGQuality JPEG 重编码质量（0-100）。
	// 85 在视觉质量与文件大小之间取得良好平衡；视觉模型对轻微失真不敏感。
	mediaOptJPEGQuality = 85
)

// normalizeInferRequest 对推理请求中所有消息的 ImagePart 做统一预处理。
// 调用方：InferenceRouter.Infer / StreamInfer（在分发给具体 Provider 前调用）。
// 副作用：就地修改 req.Messages 中的 ImagePart.Data 和 MediaType（仅图片，不触碰文本/工具调用）。
func normalizeInferRequest(req *types.InferRequest) {
	if req == nil {
		return
	}
	for i := range req.Messages {
		msg := &req.Messages[i]
		for j, part := range msg.Parts {
			ip, ok := part.(types.ImagePart)
			if !ok {
				continue
			}
			if !mediaOptIsProcessable(ip.MediaType) {
				// WebP / BMP 等无法用标准库解码，原样透传
				continue
			}
			newData, newMime, changed := mediaOptResize(ip.Data, ip.MediaType)
			if !changed {
				continue
			}
			slog.Debug("inference: image normalized for LLM",
				"orig_kb", len(ip.Data)/1024,
				"new_kb", len(newData)/1024,
				"orig_mime", ip.MediaType,
				"new_mime", newMime,
			)
			msg.Parts[j] = types.ImagePart{
				Type:      ip.Type,
				MediaType: newMime,
				Data:      newData,
				URL:       ip.URL, // URL 路径不受影响
			}
		}
	}
}

// mediaOptIsProcessable 判断 MIME 类型是否可被 Go 标准库解码。
// WebP（需 golang.org/x/image/webp）、BMP 等不在列，原样透传。
func mediaOptIsProcessable(mimeType string) bool {
	switch {
	case strings.HasPrefix(mimeType, "image/jpeg"),
		strings.HasPrefix(mimeType, "image/jpg"),
		strings.HasPrefix(mimeType, "image/png"),
		strings.HasPrefix(mimeType, "image/gif"):
		return true
	default:
		return false
	}
}

// mediaOptResize 对图片进行降采样和格式转换：
//  1. 最长边超过 mediaOptMaxImageSide 时等比缩放（最近邻插值）
//  2. 非 JPEG 格式转为 JPEG（quality=85）
//
// 返回 (newData, newMime, changed)：changed=false 表示无需处理，调用方应直接使用原始数据。
func mediaOptResize(data []byte, mimeType string) ([]byte, string, bool) {
	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		// 解码失败（格式损坏或不支持）→ 原样透传
		return data, mimeType, false
	}

	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()

	needsResize := w > mediaOptMaxImageSide || h > mediaOptMaxImageSide
	// PNG/GIF 转 JPEG；JPEG 本身只在需要 resize 时才重编码
	needsConvert := format == "png" || format == "gif"

	if !needsResize && !needsConvert {
		return data, mimeType, false
	}

	if needsResize {
		var newW, newH int
		if w >= h {
			newW = mediaOptMaxImageSide
			newH = h * mediaOptMaxImageSide / w
		} else {
			newH = mediaOptMaxImageSide
			newW = w * mediaOptMaxImageSide / h
		}
		if newW < 1 {
			newW = 1
		}
		if newH < 1 {
			newH = 1
		}
		img = mediaOptScaleNearest(img, newW, newH)
	}

	// 消除 alpha 通道（JPEG 不支持透明度），用白色背景合成
	dst := mediaOptFlattenToRGB(img)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: mediaOptJPEGQuality}); err != nil {
		// 编码失败 → 原样透传，不中断推理流程
		return data, mimeType, false
	}
	return buf.Bytes(), "image/jpeg", true
}

// mediaOptScaleNearest 最近邻插值降采样。
// 对"送给 LLM 的图片"场景，最近邻精度完全够用，且无需任何外部依赖。
// 时间复杂度 O(newW × newH)，对 1568×1568 目标约 2.4M 次赋值，典型耗时 < 5ms。
func mediaOptScaleNearest(src image.Image, newW, newH int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	srcBounds := src.Bounds()
	srcW := srcBounds.Dx()
	srcH := srcBounds.Dy()

	for y := 0; y < newH; y++ {
		for x := 0; x < newW; x++ {
			// 映射目标像素到最近源像素（整数取整）
			srcX := x*srcW/newW + srcBounds.Min.X
			srcY := y*srcH/newH + srcBounds.Min.Y
			dst.Set(x, y, src.At(srcX, srcY))
		}
	}
	return dst
}

// mediaOptFlattenToRGB 将图片合成到白色背景的 RGBA 画布，消除 alpha 通道。
// PNG/GIF 的透明像素在白色背景上呈现，避免 JPEG 编码时 alpha 丢失导致色彩错乱。
func mediaOptFlattenToRGB(src image.Image) *image.RGBA {
	bounds := src.Bounds()
	dst := image.NewRGBA(bounds)

	// 先填白色背景
	draw.Draw(dst, bounds, image.White, image.Point{}, draw.Src)
	// 叠加源图像（Over 混合：透明区域透出白色）
	draw.Draw(dst, bounds, src, bounds.Min, draw.Over)
	return dst
}
