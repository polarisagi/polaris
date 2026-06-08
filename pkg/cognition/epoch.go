package cognition

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync/atomic"
)

// ContextFingerprint 是 ContextLayout 各 zone content 的 SHA256 摘要，
// 用于检测连续上下文构建之间内容是否发生变化。
type ContextFingerprint string

// ComputeLayoutFingerprint 遍历 ContextLayout 各 zone，计算联合摘要。
// 每个 zone 的 Content 和结构元数据（MaxTokens / Tainted / Output 标志）参与计算，
// 确保即使内容相同但布局策略变化也能检测到。
func ComputeLayoutFingerprint(layout *ContextLayout) ContextFingerprint {
	h := sha256.New()
	for _, z := range layout.zones {
		_, _ = h.Write([]byte(z.Content))
		_, _ = fmt.Fprintf(h, "\x00zone:%d:%v:%v\x00", z.MaxTokens, z.Tainted, z.Output)
	}
	return ContextFingerprint(hex.EncodeToString(h.Sum(nil)))
}

// EpochTracker 跟踪上下文版本，每次检测到指纹不同时自动递增 epoch。
//
// 用法:
//
//	tracker := NewEpochTracker()
//	layout := BuildContext(wm, maxTokens)
//	// ... 填充各 zone Content ...
//	fp := ComputeLayoutFingerprint(layout)
//	layout.Epoch = tracker.Check(fp)
//
// EpochTracker 可安全并发使用。
type EpochTracker struct {
	lastFingerprint atomic.Value // ContextFingerprint
	epoch           atomic.Int64
}

// NewEpochTracker 创建 tracke，epoch 初始为 1。
func NewEpochTracker() *EpochTracker {
	t := &EpochTracker{}
	t.epoch.Store(1)
	return t
}

// Check 返回当前 epoch。若指纹与上次 Check 不同，自动递增 epoch 并返回新值。
func (t *EpochTracker) Check(fp ContextFingerprint) int64 {
	last, ok := t.lastFingerprint.Load().(ContextFingerprint)
	if ok && last == fp {
		return t.epoch.Load()
	}
	t.lastFingerprint.Store(fp)
	return t.epoch.Add(1)
}

// CurrentEpoch 返回当前 epoch 值，不变更状态。
func (t *EpochTracker) CurrentEpoch() int64 {
	return t.epoch.Load()
}
