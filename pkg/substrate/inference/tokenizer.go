package inference

import (
	"bytes"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"strings"
	"sync"

	tiktoken "github.com/pkoukk/tiktoken-go"

	"github.com/polarisagi/polaris/internal/protocol"
)

// tiktokenTokenizer 使用 tiktoken BPE 精确计算 token 数。
// 支持 o200k_base（GPT-4o/o1/o3）和 cl100k_base（GPT-4/GPT-3.5/DeepSeek）。
// 首次使用时懒加载词汇文件（需网络或 TIKTOKEN_CACHE_DIR），失败后 fallback 到 len/4。
type tiktokenTokenizer struct {
	once    sync.Once
	enc     *tiktoken.Tiktoken
	encName string
}

var _ protocol.MultimodalTokenizer = (*tiktokenTokenizer)(nil)

func newTiktokenTokenizer(model string) *tiktokenTokenizer {
	return &tiktokenTokenizer{encName: encodingForModel(model)}
}

// encodingForModel 按 OpenAI 2024/2025 编码表映射模型名。
// o200k_base: GPT-4o / o1 / o3 系列。
// cl100k_base: GPT-4 / GPT-3.5-Turbo / DeepSeek（与 cl100k_base 高度兼容）。
func encodingForModel(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "o1"), strings.Contains(m, "o3"),
		strings.Contains(m, "gpt-4o"), strings.Contains(m, "o200k"):
		return "o200k_base"
	default:
		return "cl100k_base"
	}
}

func (t *tiktokenTokenizer) getEncoder() *tiktoken.Tiktoken {
	t.once.Do(func() {
		enc, err := tiktoken.GetEncoding(t.encName)
		if err == nil {
			t.enc = enc
		}
		// 离线或下载失败：t.enc 保持 nil，各方法 fallback 到字符估算
	})
	return t.enc
}

// CountTokens 精确计算文本 token 数；离线环境 fallback 到 len/4。
func (t *tiktokenTokenizer) CountTokens(text string) int {
	enc := t.getEncoder()
	if enc == nil {
		return len([]rune(text)) / 3 // 中文 1 字符≈1 token，英文 4 字符≈1 token，取折中
	}
	return len(enc.Encode(text, nil, nil))
}

// CountTokensBatch 批量计算；共享同一个 enc 实例，无需重复加锁。
func (t *tiktokenTokenizer) CountTokensBatch(texts []string) []int {
	result := make([]int, len(texts))
	for i, s := range texts {
		result[i] = t.CountTokens(s)
	}
	return result
}

// CountImageTokens 按 OpenAI GPT-4V tile 规则计算图片 token 数。
// detail="low": 固定 85 tokens。
// detail="high"/"auto"/"": tile 公式。
// width/height=0: 用默认 1024×1024 估算。
func (t *tiktokenTokenizer) CountImageTokens(width, height int, detail string) int {
	if detail == "low" {
		return 85
	}
	if width <= 0 || height <= 0 {
		// 尺寸未知，按常见上传图片 1024×1024 估算
		return imageTokensByTile(1024, 1024)
	}
	return imageTokensByTile(width, height)
}

// imageTokensByTile 按 OpenAI GPT-4V 文档计算 high-detail 图片 token：
// 1. 缩放到 2048×2048 以内（保持纵横比）
// 2. 缩放最短边到 768px（保持纵横比）
// 3. 每个 512×512 tile = 170 tokens；基础开销 85 tokens
// 参考: https://platform.openai.com/docs/guides/vision#calculating-costs
func imageTokensByTile(width, height int) int {
	w, h := float64(width), float64(height)

	// 步骤 1
	if w > 2048 || h > 2048 {
		scale := math.Min(2048/w, 2048/h)
		w, h = w*scale, h*scale
	}
	// 步骤 2
	shortest := math.Min(w, h)
	if shortest > 768 {
		scale := 768 / shortest
		w, h = w*scale, h*scale
	}
	// 步骤 3
	tilesW := int(math.Ceil(w / 512))
	tilesH := int(math.Ceil(h / 512))
	return tilesW*tilesH*170 + 85
}

// CountImageBytesTokens 从 image 原始字节解析尺寸后计算 token 数（PNG/JPEG/GIF/WebP）。
// 解析失败时用默认 1024×1024 估算。
func (t *tiktokenTokenizer) CountImageBytesTokens(data []byte, detail string) int {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return t.CountImageTokens(0, 0, detail)
	}
	return t.CountImageTokens(cfg.Width, cfg.Height, detail)
}

// CountVideoTokens 估算视频 token 数（OpenAI 帧采样规则）。
// 默认 fps=1（OpenAI 视频理解按 1fps 采样），每帧等价于 512×512 high-detail 图片（255 tokens）。
func (t *tiktokenTokenizer) CountVideoTokens(durationSecs float64, fps float64) int {
	if fps <= 0 {
		fps = 1.0
	}
	frames := int(math.Ceil(durationSecs * fps))
	if frames <= 0 {
		frames = 1
	}
	// 512×512 high detail: ceil(512/512)*ceil(512/512)*170+85 = 255
	return frames * 255
}

// EstimateRequest 估算整个 InferRequest 的输入 token 数（含文本+多模态）。
// 用于流式请求取消时的补偿计费（精确值来自 API usage，此处为客户端预估）。
//
// OpenAI chat format 每条消息固定开销：4 tokens（role overhead）；reply priming：3 tokens。
func (t *tiktokenTokenizer) EstimateRequest(req *protocol.InferRequest) int {
	total := 0
	for _, msg := range req.Messages {
		total += 4 // <|im_start|>role\n + <|im_end|>
		if len(msg.Parts) > 0 {
			for _, p := range msg.Parts {
				switch part := p.(type) {
				case protocol.ImagePart:
					if len(part.Data) > 0 {
						total += t.CountImageBytesTokens(part.Data, part.Detail)
					} else {
						total += t.CountImageTokens(part.Width, part.Height, part.Detail)
					}
				case protocol.VideoPart:
					// 无法从 URI/Data 精确获取时长，按保守 10 秒 1fps 估算
					total += t.CountVideoTokens(10, 1)
				case map[string]any:
					if txt, ok := part["text"].(string); ok {
						total += t.CountTokens(txt)
					}
				}
			}
		} else {
			total += t.CountTokens(msg.Content)
		}
		if msg.ReasoningContent != "" {
			total += t.CountTokens(msg.ReasoningContent)
		}
	}
	total += 3 // reply priming: <|im_start|>assistant
	return total
}
