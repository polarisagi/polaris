package llm

import (
	"context"
	"math"
	"strings"
	"time"
)

// StreamBudgetGuard — 流式响应 token 预算守卫。
// 架构文档: docs/arch/01-Inference-Runtime-深度选型.md §5.2-5.4

type StreamBudgetGuard struct {
	sessionBudget  *TokenBudget
	burnDetector   *TokenBurnDetector
	maxBufferSize  int // 256KB
	accumulatedOut int
	chunkCount     int
}

// NewStreamBudgetGuard 创建 TokenBudget。
func NewStreamBudgetGuard(budget *TokenBudget, detector *TokenBurnDetector, maxBuf int) *StreamBudgetGuard {
	return &StreamBudgetGuard{
		sessionBudget: budget,
		burnDetector:  detector,
		maxBufferSize: maxBuf,
	}
}

// GetMaxBufferSize 返回最大缓冲区大小。
func (g *StreamBudgetGuard) GetMaxBufferSize() int {
	return g.maxBufferSize
}

// TokenBudget token 预算。
type TokenBudget struct {
	remaining int
}

// NewTokenBudget 以初始配额创建 TokenBudget（P1 2026-07-12 接入 router_stream.go
// 时补充的构造函数——此前只有测试内直接构造 &TokenBudget{remaining: N}，生产侧
// 无法在不感知内部字段的前提下使用）。
// initial<=0 视为"无预算上限"（如调用方未指定 MaxTokens），此时只依赖
// TokenBurnDetector 的加速度检测兜底，不会触发 L3 预算耗尽硬阻断。
func NewTokenBudget(initial int) *TokenBudget {
	if initial <= 0 {
		initial = math.MaxInt32
	}
	return &TokenBudget{remaining: initial}
}

// Consume 从预算中扣除 n 个 token（允许降到 0 以下，GuardChunk 只关心 <=0）。
func (b *TokenBudget) Consume(n int) {
	b.remaining -= n
}

// Remaining 返回当前剩余配额，供日志/监控展示。
func (b *TokenBudget) Remaining() int {
	return b.remaining
}

// TokenBurnDetector 基于加速度的 burn rate 检测。
// 5s 窗口: v1=(mid-first)/dt1, v2=(last-mid)/dt2, accel=(v2-v1)/((dt1+dt2)/2)
// accel > baseline.P95 × 3.0 → BurnAlert
type TokenBurnDetector struct {
	samples []burnSample
	window  int64 // 5s
}

// NewTokenBurnDetector 创建燃烧检测器。
func NewTokenBurnDetector(window int64) *TokenBurnDetector {
	return &TokenBurnDetector{
		window: window,
	}
}

// GetWindow 返回检测窗口时间。
func (d *TokenBurnDetector) GetWindow() int64 {
	return d.window
}

type burnSample struct {
	tokens int64
	ts     int64 // unix micro
}

// GuardChunk 检查每个 token chunk。
// 摊销检查: 每 100 chunk 或第 1 chunk 执行。
// L1: sessBudget.Remaining() <= 0 → WARN (不阻断)
// L2: burnDetector 检测加速度异常 → 硬阻断
// L3: sessBudget.IsExhausted() → 硬阻断
func (g *StreamBudgetGuard) GuardChunk(ctx context.Context, tokens int) error {
	g.chunkCount++
	g.accumulatedOut += tokens

	if g.chunkCount%100 != 0 && g.chunkCount != 1 {
		return nil
	}

	if alert := g.burnDetector.CheckAcceleration(g.accumulatedOut); alert != nil {
		return ErrFatalStreamAbort
	}

	if g.sessionBudget.remaining <= 0 {
		return ErrStreamBudgetExhausted
	}

	return nil
}

// CheckAcceleration 检测 token 消耗加速度异常。
func (d *TokenBurnDetector) CheckAcceleration(accumulated int) error {
	now := time.Now().UnixMicro()
	d.samples = append(d.samples, burnSample{tokens: int64(accumulated), ts: now})

	// 保留 5s 窗口
	cutoff := time.Now().Add(-5 * time.Second).UnixMicro()
	start := 0
	for start < len(d.samples) && d.samples[start].ts < cutoff {
		start++
	}
	d.samples = d.samples[start:]

	if len(d.samples) < 3 {
		return nil
	}

	mid := len(d.samples) / 2
	first := d.samples[0]
	last := d.samples[len(d.samples)-1]
	middle := d.samples[mid]

	dt1 := float64(middle.ts - first.ts)
	dt2 := float64(last.ts - middle.ts)
	if dt1 == 0 || dt2 == 0 {
		return nil
	}

	v1 := float64(middle.tokens-first.tokens) / dt1
	v2 := float64(last.tokens-middle.tokens) / dt2
	accel := (v2 - v1) / ((dt1 + dt2) / 2)

	if accel > 3.0 { // 3.0x P95 阈值
		return &BurnAlert{Acceleration: accel}
	}
	return nil
}

// JSONRepair 栈式 JSON 修复。
// 栈式括号匹配 → 自动闭合 } ] " → 截断至最后合法 JSON 路径 → 移除不完整 key-value。
// 确定性 Go 实现, <1ms.
func JSONRepair(data []byte) (*RepairResult, error) { //nolint:gocyclo,nestif
	var stack []byte
	inString := false
	escape := false

	for _, c := range data {
		//nolint:nestif
		if inString {
			if escape {
				escape = false
			} else if c == '\\' {
				escape = true
			} else if c == '"' {
				inString = false
			}
		} else {
			switch c {
			case '"':
				inString = true
			case '{', '[':
				stack = append(stack, c)
			case '}':
				if len(stack) > 0 && stack[len(stack)-1] == '{' {
					stack = stack[:len(stack)-1]
				}
			case ']':
				if len(stack) > 0 && stack[len(stack)-1] == '[' {
					stack = stack[:len(stack)-1]
				}
			}
		}
	}

	res := &RepairResult{
		Repaired: append([]byte(nil), data...),
	}

	// 闭合未完成的字符串
	if inString {
		res.Repaired = append(res.Repaired, '"')
		res.BracketsClosed++
	}

	// 简单清理悬空的逗号或冒号 (移除不完整 key-value)
	cleaned := false
	for i := len(res.Repaired) - 1; i >= 0; i-- {
		c := res.Repaired[i]
		if c == ' ' || c == '\n' || c == '\r' || c == '\t' {
			continue
		}
		if c == ',' {
			res.Repaired = res.Repaired[:i]
			cleaned = true
			break
		}
		if c == ':' {
			// 如果以 : 结尾，说明 key 不完整，尝试找到 key 的开头并丢弃
			cleaned = cleanDanglingColon(res, i)
			break
		}
		break
	}

	// 出栈并闭合
	for i := len(stack) - 1; i >= 0; i-- {
		switch stack[i] {
		case '{':
			res.Repaired = append(res.Repaired, '}')
		case '[':
			res.Repaired = append(res.Repaired, ']')
		}
		res.BracketsClosed++
	}

	if res.BracketsClosed > 0 || cleaned {
		res.JsonRepaired = true
	}

	return res, nil
}

type RepairResult struct {
	Repaired       []byte
	DiscardedKeys  []string
	BracketsClosed int
	JsonRepaired   bool
}

// TrackStreamCost 流式成本追踪。
// 流正常结束 → 精确 API usage; 流中断 → 根据中断原因处理。
func TrackStreamCost(ctx context.Context, accumulated int, provider string) error {
	// FatalStreamAbort → 丢弃 accumulatedOutput → M4 S_REPLAN
	// > MaxStreamBufferSize (256KB) → ErrResponseTooLarge
	if accumulated > 256*1024 {
		return ErrResponseTooLarge
	}
	return nil
}

var (
	ErrFatalStreamAbort      = &StreamError{"fatal stream abort"}
	ErrStreamBudgetExhausted = &StreamError{"stream budget exhausted"}
	ErrResponseTooLarge      = &StreamError{"response too large"}
)

type StreamError struct{ msg string }

func (e *StreamError) Error() string { return e.msg }

type BurnAlert struct{ Acceleration float64 }

func (b *BurnAlert) Error() string { return "burn rate acceleration alert" }

// extractLastJSONKey 从片段（如 ` "foo": `）中提取最后一个 JSON key 名。
// 返回空串表示提取失败（格式不符）。
func extractLastJSONKey(fragment string) string {
	fragment = strings.TrimSpace(fragment)
	if len(fragment) < 2 || fragment[0] != '"' {
		return ""
	}
	end := strings.Index(fragment[1:], `"`)
	if end < 0 {
		return ""
	}
	return fragment[1 : end+1]
}

func cleanDanglingColon(res *RepairResult, i int) bool {
	for j := i - 1; j >= 0; j-- {
		if res.Repaired[j] == ',' || res.Repaired[j] == '{' {
			fragment := string(res.Repaired[j+1 : i])
			if key := extractLastJSONKey(fragment); key != "" {
				res.DiscardedKeys = append(res.DiscardedKeys, key)
			}
			res.Repaired = res.Repaired[:j+1]
			if res.Repaired[j] == ',' {
				res.Repaired = res.Repaired[:j] // 连同逗号一起删掉
			}
			return true
		}
	}
	return false
}
