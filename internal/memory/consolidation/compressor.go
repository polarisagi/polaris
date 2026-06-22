package consolidation

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/memory/graph"

	"github.com/polarisagi/polaris/internal/protocol"
)

// ToolRefOffloader 定义工具长输出的安全落盘接口
type ToolRefOffloader interface {
	Offload(id string, data []byte) error
}

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
// 对应架构概念：L0 极速工作记忆 (Working Memory) 的符号化卸载层 (Symbolic Offloading Layer)。
// 存根格式 "[offloaded: N bytes → read_tool_ref("xxx")]" 即 <log_ref> 符号指针。
// Stage 3 Mermaid 注入对应 TencentDB-Agent PruneMem 机制。
//
// Stage 1 — tool output pre-pruning: 将超阈值 tool_result 替换为存根，立即释放 token。
// Stage 2 — LLM 锚点摘要（由上层 LLM 调用填充 anchor 字段）。
// Stage 3 — Mermaid 任务状态注入: 将 TaskMermaidCanvas 渲染结果嵌入 anchor 前缀，
//
//	使 LLM 在压缩后仍能理解"当前走到哪一步、哪步失败了"。
//
// 来源: TencentDB Agent Memory 符号化短期记忆（61% token 节省原理）。
//
// 防抖动: 两次压缩之间强制 antithrashCooldown 冷却期，
// 避免 compress→expand→compress 振荡（hermes-agent anti-thrashing 机制）。
type SessionCompressor struct {
	maxSummaryTokens   int
	maxToolOutputBytes int
	triggerPct         float64
	anchor             string                   // 锚点摘要（架构决策/失败原因/修复方案/风格偏好 永久保留）
	DurativeMemory     map[string]any           // 持久化核心记忆对象
	offloader          ToolRefOffloader         // P0: 工具输出卸载器
	canvas             *graph.TaskMermaidCanvas // 任务状态 Mermaid 画布（缺口 2）
	memPressure        *atomic.Int32            // 由 GovernanceAgent 注入，nil 时忽略
	outboxWriter       protocol.OutboxWriter    // Outbox 写入器，用于触发后续异步任务

	mu             sync.Mutex
	lastCompressAt time.Time
}

// InjectOffloader 注入 ToolRefOffloader 以开启文件落盘
func (sc *SessionCompressor) InjectOffloader(o ToolRefOffloader) {
	sc.mu.Lock()
	sc.offloader = o
	sc.mu.Unlock()
}

// InjectMemPressure 注入内存压力指针（由顶层 wire 调用）。
func (sc *SessionCompressor) InjectMemPressure(p *atomic.Int32) {
	sc.mu.Lock()
	sc.memPressure = p
	sc.mu.Unlock()
}

// InjectOutboxWriter 注入 Outbox 写入器
func (sc *SessionCompressor) InjectOutboxWriter(w protocol.OutboxWriter) {
	sc.mu.Lock()
	sc.outboxWriter = w
	sc.mu.Unlock()
}

// NewSessionCompressor 创建压缩器。maxSummaryTokens 为 Stage 2 LLM 摘要的 token 预算。
func NewSessionCompressor(maxSummaryTokens int) *SessionCompressor {
	return &SessionCompressor{
		maxSummaryTokens:   maxSummaryTokens,
		maxToolOutputBytes: defaultMaxToolOutputBytes,
		triggerPct:         defaultTriggerPct,
		canvas:             graph.NewTaskMermaidCanvas(),
	}
}

// TrackToolCall 通知压缩器：工具调用已发起（pending 状态）。
// 应由 Agent Kernel 在发送 tool_use 消息后立即调用。
func (sc *SessionCompressor) TrackToolCall(toolUseID, toolName string) {
	sc.canvas.TrackToolCall(toolUseID, toolName)
}

// TrackToolResult 通知压缩器：工具调用已完成（success/failed 状态）。
// summary 为 ≤40 字的执行结果摘要，由调用方提供。
func (sc *SessionCompressor) TrackToolResult(toolUseID string, success bool, summary string) {
	sc.canvas.TrackToolResult(toolUseID, success, summary)
}

// Canvas 返回当前 Mermaid 画布（供调试和测试使用）。
func (sc *SessionCompressor) Canvas() *graph.TaskMermaidCanvas {
	return sc.canvas
}

// ResetCanvas 清空任务状态画布（任务边界切换时调用）。
func (sc *SessionCompressor) ResetCanvas() {
	sc.canvas.Reset()
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

	// 内存压力高时降低触发阈值，提前启动压缩
	if sc.memPressure != nil {
		switch sc.memPressure.Load() {
		case 1: // MemPressureModerate
			thr = thr * 0.65
		case 2: // MemPressureCritical
			thr = thr * 0.35
		}
	}

	return float64(currentTokens)/float64(maxTokens) >= thr
}

// Compress 执行三阶段压缩。
//
// Stage 1 — tool output pre-pruning: 超阈值 tool_result 替换为 node_id 存根。
// Stage 2 — LLM 锚点摘要（由上层填充 anchor 字段）。
// Stage 3 — Mermaid 画布前缀注入: 将任务执行状态图嵌入 anchor，使 LLM 压缩后
//
//	仍能理解"走到哪步、哪步失败"（TencentDB 符号化记忆核心机制）。
//
// 返回修改后的消息列表和是否实际执行了压缩。
// false 表示未达阈值，或被防抖动冷却期拦截。
func (sc *SessionCompressor) Compress(messages []types.Message, currentTokens, maxTokens int) ([]types.Message, bool) {
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

	// Stage 1: tool output pre-pruning（含 offload 落盘）
	maxBytes := sc.maxToolOutputBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxToolOutputBytes
	}
	pruned, _ := PruneToolOutputs(messages, maxBytes, sc.offloader, sc.outboxWriter)

	// Stage 3: 将 Mermaid 画布渲染结果注入 anchor 前缀
	// 若画布有节点，则在 anchor 开头注入 "## Task State\n{mermaid}" 块，
	// 后接原 LLM anchor 摘要（若有）。
	sc.mu.Lock()
	if mmd := sc.canvas.Render(); mmd != "" {
		prefix := "## Task State (node_id → read_tool_ref for raw output)\n" + mmd
		if sc.anchor != "" {
			sc.anchor = prefix + "\n## Summary\n" + sc.anchor
		} else {
			sc.anchor = prefix
		}
	}
	sc.mu.Unlock()

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
// 实现 Symbolic Offloading：将超阈值工具输出替换为符号指针，保护 L0 工作记忆。
// 原 messages 切片不被修改；返回新切片和被裁剪的 tool_result 条数。
//
// 存根格式: "[offloaded: N bytes → read_tool_ref("xxx")]"
// content 支持两种 Anthropic 格式: string 和 []any（多块内容数组）。
func PruneToolOutputs(messages []types.Message, maxBytes int, offloader ToolRefOffloader, outboxWriter protocol.OutboxWriter) ([]types.Message, int) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxToolOutputBytes
	}
	out := make([]types.Message, len(messages))
	copy(out, messages)
	total := 0
	for i, msg := range messages {
		if len(msg.Parts) == 0 {
			continue
		}
		if parts, modified := prunePartsToolOutputs(msg.Parts, maxBytes, offloader, outboxWriter, &total); modified {
			out[i].Parts = parts
		}
	}
	return out, total
}

// prunePartsToolOutputs 处理单条消息的 Parts。
// 返回 (新切片, true) 表示有修改；(nil, false) 表示无修改，调用方无需替换。
func prunePartsToolOutputs(parts []any, maxBytes int, offloader ToolRefOffloader, outboxWriter protocol.OutboxWriter, counter *int) ([]any, bool) {
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

		// P0: 卸载原始长输出
		//nolint:nestif
		if offloader != nil {
			var rawData []byte
			switch v := m["content"].(type) {
			case string:
				rawData = []byte(v)
			case []any:
				rawData, _ = json.Marshal(v)
			}
			if len(rawData) > 0 {
				if err := offloader.Offload(id, rawData); err == nil {
					// 若卸载的内容疑似为错误堆栈（启发式检测），触发语义压缩
					if outboxWriter != nil && looksLikeErrorStack(rawData) {
						_ = outboxWriter.Write(context.Background(), protocol.OutboxEntry{
							TargetEngine:   "semantic_compress",
							Operation:      "compress",
							Scope:          "error_stack",
							Payload:        []byte(`{"vfs_id":"` + id + `"}`),
							IdempotencyKey: "semantic_compress:error_stack:" + id,
						})
					}
				}
			}
		}

		newParts[i] = map[string]any{
			"type":        "tool_result",
			"tool_use_id": id,
			"content":     fmt.Sprintf("[offloaded: %d bytes → read_tool_ref(%q)]", size, id),
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
func EstimateImageTokens(messages []types.Message) int {
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

// looksLikeErrorStack 启发式检测长文本是否为错误堆栈
func looksLikeErrorStack(data []byte) bool {
	s := string(data)
	if len(s) < 20 {
		return false
	}
	return containsAny(s, "panic:", "stack trace", "goroutine ", "Error:")
}

func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
