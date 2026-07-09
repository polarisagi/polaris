package lam

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// StreamingActionBus 是 LAM 连续动作流的速率限制和裁剪总线。
// 架构文档: docs/arch/07-Tool-Action-Layer-深度选型.md §7.3
//
// [接口预留][实现依赖 Tier-1+ 显示服务器接入，当前为令牌桶速率控制实现]
type StreamingActionBus struct {
	displayServer DisplayServer
	rateLimiter   *ActionRateLimiter
	clipper       *ActionClipper
	maxSteps      int // 1000 (~16s @60fps)

	mu        sync.Mutex
	stepCount int
}

// DisplayServer 抽象显示后端（Xvfb / VNC / Wayland）。
type DisplayServer interface {
	SendAction(action any) error
	GetFrame() ([]byte, error)
}

// ActionRateLimiter 滑动窗口 + 令牌桶混合速率限制。
type ActionRateLimiter struct {
	maxActionsPerWindow int
	windowDurationMs    int
	tokensPerSec        float64

	mu         sync.Mutex
	tokens     float64
	lastRefill time.Time
	windowHits int
	windowEnd  time.Time
}

// ActionClipper 每维 [min, max] 钳制。
type ActionClipper struct {
	mins []float64
	maxs []float64
}

// NewStreamingActionBus 创建动作流总线。
//
//	maxSteps:         最大允许步数（0 = 默认 1000）
//	maxActionsPerSec: 每秒最大动作数（令牌桶速率，0 = 默认 60）
func NewStreamingActionBus(displayServer DisplayServer, maxSteps int, maxActionsPerSec float64) *StreamingActionBus {
	if maxSteps <= 0 {
		maxSteps = 1000
	}
	if maxActionsPerSec <= 0 {
		maxActionsPerSec = 60
	}
	return &StreamingActionBus{
		displayServer: displayServer,
		maxSteps:      maxSteps,
		rateLimiter: &ActionRateLimiter{
			maxActionsPerWindow: int(maxActionsPerSec * 10),
			windowDurationMs:    10000,
			tokensPerSec:        maxActionsPerSec,
			tokens:              maxActionsPerSec,
			lastRefill:          time.Now(),
			windowEnd:           time.Now().Add(10 * time.Second),
		},
	}
}

// WithClipper 设置动作向量裁剪器（链式调用）。
func (b *StreamingActionBus) WithClipper(mins, maxs []float64) *StreamingActionBus {
	b.clipper = &ActionClipper{mins: mins, maxs: maxs}
	return b
}

// StreamAction 将连续动作发送到显示后端。
//
// 执行流程：
//  1. maxSteps 步数检查
//  2. 令牌桶速率控制（阻塞直到获得令牌，或 ctx 取消）
//  3. 若有 clipper，执行向量钳制
//  4. 调用 DisplayServer.SendAction
//
// [接口预留][DisplayServer 实现待 Tier-1+ 平台驱动接入]
func (b *StreamingActionBus) StreamAction(ctx context.Context, action ContinuousAction) error {
	b.mu.Lock()
	if b.stepCount >= b.maxSteps {
		b.mu.Unlock()
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("streaming_action_bus: max steps (%d) reached", b.maxSteps))
	}
	b.stepCount++
	b.mu.Unlock()

	// 令牌桶速率控制
	if err := b.rateLimiter.Acquire(ctx); err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("streaming_action_bus: rate limit: %v", err), err)
	}

	// 向量钳制
	vec := action.ActionVector
	if b.clipper != nil {
		vec = b.clipper.Clip(vec)
	}

	// 若 DisplayServer 未接入，以 no-op 方式处理（接口预留安全降级）
	if b.displayServer == nil {
		return nil
	}

	return b.displayServer.SendAction(map[string]any{
		"type":       action.ActionType,
		"vector":     vec,
		"confidence": action.Confidence,
	})
}

// StepCount 返回当前已发送的动作步数。
func (b *StreamingActionBus) StepCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stepCount
}

// Reset 重置步数计数器（新 episode 开始时调用）。
func (b *StreamingActionBus) Reset() {
	b.mu.Lock()
	b.stepCount = 0
	b.mu.Unlock()
}

// Acquire 从令牌桶获取一个令牌（阻塞直到可用或 ctx 取消）。
func (rl *ActionRateLimiter) Acquire(ctx context.Context) error {
	for {
		rl.mu.Lock()
		now := time.Now()

		// 补充令牌
		elapsed := now.Sub(rl.lastRefill).Seconds()
		rl.tokens += elapsed * rl.tokensPerSec
		if rl.tokens > rl.tokensPerSec {
			rl.tokens = rl.tokensPerSec // 上限 = 1s 容量
		}
		rl.lastRefill = now

		// 滑动窗口检查
		if now.After(rl.windowEnd) {
			rl.windowHits = 0
			rl.windowEnd = now.Add(time.Duration(rl.windowDurationMs) * time.Millisecond)
		}

		if rl.tokens >= 1 && rl.windowHits < rl.maxActionsPerWindow {
			rl.tokens--
			rl.windowHits++
			rl.mu.Unlock()
			return nil
		}

		// 计算等待时间
		waitUntilToken := time.Duration((1-rl.tokens)/rl.tokensPerSec*1000) * time.Millisecond
		waitUntilWindow := rl.windowEnd.Sub(now)
		wait := waitUntilToken
		if waitUntilWindow < wait {
			wait = waitUntilWindow
		}
		if wait < time.Millisecond {
			wait = time.Millisecond
		}
		rl.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// Clip 按维度钳制动作向量。
func (c *ActionClipper) Clip(vec []float64) []float64 {
	out := make([]float64, len(vec))
	for i, v := range vec {
		out[i] = v
		if i < len(c.mins) && v < c.mins[i] {
			out[i] = c.mins[i]
		}
		if i < len(c.maxs) && v > c.maxs[i] {
			out[i] = c.maxs[i]
		}
	}
	return out
}
