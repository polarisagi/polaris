package retrieval

import (
	"sync"
	"time"
)

// EmbedBackoff 管理后台向量化任务（reindexer 与插件向量回填）的重试退避器 (ADR-0109 D5)。
// 策略：连续失败 3 次后下一次等待依次 5m -> 30m -> 2h，再失败则停止重试，
// 直到后端变更 (Reset()) 或手动触发。
type EmbedBackoff struct {
	mu        sync.Mutex
	failures  int
	stopped   bool
	nextRetry time.Time
	nowFn     func() time.Time // 用于单测注入时间
}

// NewEmbedBackoff 创建一个初始状态的后台向量任务退避器。
func NewEmbedBackoff() *EmbedBackoff {
	return &EmbedBackoff{
		nowFn: time.Now,
	}
}

// NextDelay 根据当前失败次数计算出的退避等待时长。
// 失败 0, 1, 2 次：等待 0（正常调度）
// 失败 3 次：5m
// 失败 4 次：30m
// 失败 5 次：2h
// 失败 >= 6 次：停止重试 (stopped=true)
func (b *EmbedBackoff) NextDelay(failures int) (time.Duration, bool) {
	switch failures {
	case 0, 1, 2:
		return 0, false
	case 3:
		return 5 * time.Minute, false
	case 4:
		return 30 * time.Minute, false
	case 5:
		return 2 * time.Hour, false
	default:
		return 0, true // stopped
	}
}

// RecordFailure 记录一次失败并计算下次重试时间。
// 返回下次等待时长、是否已停止重试以及连续失败次数。
func (b *EmbedBackoff) RecordFailure() (delay time.Duration, stopped bool, failures int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.failures++
	delay, stopped = b.NextDelay(b.failures)
	b.stopped = stopped
	if !stopped && delay > 0 {
		now := b.now()
		b.nextRetry = now.Add(delay)
	} else if stopped {
		b.nextRetry = time.Time{}
	}
	return delay, b.stopped, b.failures
}

// RecordSuccess 记录一次成功，清空连续失败计数与退避状态。
func (b *EmbedBackoff) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.failures = 0
	b.stopped = false
	b.nextRetry = time.Time{}
}

// Reset 强制重置退避器（如嵌入后端热切换 dynEmbedder.Set 时触发）。
func (b *EmbedBackoff) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.failures = 0
	b.stopped = false
	b.nextRetry = time.Time{}
}

// CanAttempt 检查当前是否允许发起尝试。
func (b *EmbedBackoff) CanAttempt() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.stopped {
		return false
	}
	if b.nextRetry.IsZero() {
		return true
	}
	return !b.now().Before(b.nextRetry)
}

// ConsecutiveFailures 返回当前连续失败次数。
func (b *EmbedBackoff) ConsecutiveFailures() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failures
}

// NextRetryAt 返回下次重试时间。
func (b *EmbedBackoff) NextRetryAt() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.nextRetry
}

// IsStopped 返回是否已达到最大失败次数并停止。
func (b *EmbedBackoff) IsStopped() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stopped
}

func (b *EmbedBackoff) now() time.Time {
	if b.nowFn != nil {
		return b.nowFn()
	}
	return time.Now()
}
