package cognition

import (
	"fmt"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

// ─── 压缩阈值常量 ─────────────────────────────────────────────────────────────

const (
	defaultMaxToolOutputBytes = 10 * 1024        // 10 KB
	defaultTriggerPct         = 0.65             // 65% 窗口满时触发
	antithrashCooldown        = 60 * time.Second // 两次压缩的最短间隔
	imageTokensFullHD         = 1445             // 1024×1024 图像 token 近似值
	imageTokensSmall          = 765              // ≤512×512 图像 token 近似值
)

// ─── SessionCompressor ────────────────────────────────────────────────────────

// SessionCompressor 两阶段上下文压缩器。
//
// Stage 1 — tool output pre-pruning: 将超阈值 tool_result 替换为存根，立即释放 token。
// Stage 2 — LLM 锚点摘要（由上层 LLM 调用填充 anchor 字段）。
//
// 防抖动: 两次压缩之间强制 antithrashCooldown 冷却期，
// 避免 compress→expand→compress 振荡（hermes-agent anti-thrashing 机制）。
type SessionCompressor struct {
	maxSummaryTokens   int
	maxToolOutputBytes int
	triggerPct         float64
	anchor             string         // 锚点摘要（架构决策/失败原因/修复方案/风格偏好 永久保留）
	DurativeMemory     map[string]any // 持久化核心记忆对象

	mu             sync.Mutex
	lastCompressAt time.Time
}

// NewSessionCompressor 创建压缩器。maxSummaryTokens 为 Stage 2 LLM 摘要的 token 预算。
func NewSessionCompressor(maxSummaryTokens int) *SessionCompressor {
	return &SessionCompressor{
		maxSummaryTokens:   maxSummaryTokens,
		maxToolOutputBytes: defaultMaxToolOutputBytes,
		triggerPct:         defaultTriggerPct,
	}
}

// ShouldTrigger 报告当前 token 用量是否达到触发阈值（默认 65%）。
func (sc *SessionCompressor) ShouldTrigger(currentTokens, maxTokens int) bool {
	if maxTokens <= 0 {
		return false
	}
	thr := sc.triggerPct
	if thr <= 0 {
		thr = defaultTriggerPct
	}
	return float64(currentTokens)/float64(maxTokens) >= thr
}

// Compress 执行两阶段压缩。
//
// 返回修改后的消息列表和是否实际执行了压缩。
// false 表示未达阈值，或被防抖动冷却期拦截。
func (sc *SessionCompressor) Compress(messages []protocol.Message, currentTokens, maxTokens int) ([]protocol.Message, bool) {
	if !sc.ShouldTrigger(currentTokens, maxTokens) {
		return messages, false
	}

	sc.mu.Lock()
	if time.Since(sc.lastCompressAt) < antithrashCooldown {
		sc.mu.Unlock()
		return messages, false
	}
	sc.lastCompressAt = time.Now()
	sc.mu.Unlock()

	// Stage 1: tool output pre-pruning
	maxBytes := sc.maxToolOutputBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxToolOutputBytes
	}
	pruned, _ := PruneToolOutputs(messages, maxBytes)
	return pruned, true
}

// Anchor 返回当前锚点摘要（可为空）。
func (sc *SessionCompressor) Anchor() string {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.anchor
}

// SetAnchor 由 Stage 2 LLM 调用写入锚点摘要。
func (sc *SessionCompressor) SetAnchor(s string) {
	sc.mu.Lock()
	sc.anchor = s
	sc.mu.Unlock()
}

// ─── Tool Output Pre-Pruning ──────────────────────────────────────────────────

// PruneToolOutputs 将消息列表中超过 maxBytes 的 tool_result 内容替换为存根。
// 原 messages 切片不被修改；返回新切片和被裁剪的 tool_result 条数。
//
// 存根格式: "[pruned: N bytes, id: <tool_use_id>]"
// content 支持两种 Anthropic 格式: string 和 []any（多块内容数组）。
func PruneToolOutputs(messages []protocol.Message, maxBytes int) ([]protocol.Message, int) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxToolOutputBytes
	}
	out := make([]protocol.Message, len(messages))
	copy(out, messages)
	total := 0
	for i, msg := range messages {
		if len(msg.Parts) == 0 {
			continue
		}
		if parts, modified := prunePartsToolOutputs(msg.Parts, maxBytes, &total); modified {
			out[i].Parts = parts
		}
	}
	return out, total
}

// prunePartsToolOutputs 处理单条消息的 Parts。
// 返回 (新切片, true) 表示有修改；(nil, false) 表示无修改，调用方无需替换。
func prunePartsToolOutputs(parts []any, maxBytes int, counter *int) ([]any, bool) {
	modified := false
	newParts := make([]any, len(parts))
	for i, p := range parts {
		m, ok := p.(map[string]any)
		if !ok || m["type"] != "tool_result" {
			newParts[i] = p
			continue
		}
		size := toolResultContentSize(m)
		if size <= maxBytes {
			newParts[i] = p
			continue
		}
		id, _ := m["tool_use_id"].(string)
		newParts[i] = map[string]any{
			"type":        "tool_result",
			"tool_use_id": id,
			"content":     fmt.Sprintf("[pruned: %d bytes, id: %s]", size, id),
		}
		*counter++
		modified = true
	}
	return newParts, modified
}

// toolResultContentSize 测量 tool_result 的 content 字节数。
// 支持 string（OpenAI/DeepSeek）和 []any（Anthropic 多块内容）两种格式。
func toolResultContentSize(m map[string]any) int {
	switch v := m["content"].(type) {
	case string:
		return len(v)
	case []any:
		total := 0
		for _, block := range v {
			if b, ok := block.(map[string]any); ok {
				if t, _ := b["text"].(string); t != "" {
					total += len(t)
				}
			}
		}
		return total
	}
	return 0
}

// ─── Image Token Estimation ───────────────────────────────────────────────────

// EstimateImageTokens 遍历消息列表，对所有 image part 估算 token 消耗并求和。
//
// 公式（Anthropic/OpenAI 发布）:
//
//	tiles  = ceil(W/512) × ceil(H/512)
//	tokens = tiles × 170 + 85
//
// 无尺寸信息时降级为保守默认值（imageTokensFullHD=1445 或 imageTokensSmall=765）。
func EstimateImageTokens(messages []protocol.Message) int {
	total := 0
	for _, msg := range messages {
		for _, p := range msg.Parts {
			m, ok := p.(map[string]any)
			if !ok || m["type"] != "image" {
				continue
			}
			total += estimateOneImage(m)
		}
	}
	return total
}

// estimateOneImage 对单个 image part 估算 token 数。
func estimateOneImage(m map[string]any) int {
	// 优先使用调用方注入的 _meta 宽高（Polaris 内部约定，非标准字段）
	if meta, ok := m["_meta"].(map[string]any); ok {
		w, wOK := meta["width"].(float64)
		h, hOK := meta["height"].(float64)
		if wOK && hOK && w > 0 && h > 0 {
			tilesW := (int(w) + 511) / 512
			tilesH := (int(h) + 511) / 512
			return tilesW*tilesH*170 + 85
		}
	}
	// 通过 source.data 长度推断：base64 > 50KB 视为全尺寸图像
	if src, ok := m["source"].(map[string]any); ok {
		if data, _ := src["data"].(string); len(data) > 50_000 {
			return imageTokensFullHD
		}
		if url, _ := src["url"].(string); url != "" {
			return imageTokensFullHD
		}
		return imageTokensSmall
	}
	return imageTokensFullHD
}
