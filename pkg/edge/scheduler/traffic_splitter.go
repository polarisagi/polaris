package scheduler

import "sync/atomic"

// TrafficSplitter 基于会话 ID 的一致性哈希流量分发，支持原子化百分比调整。
// 架构文档: docs/arch/M13-Interface-Scheduler.md §2.3
type TrafficSplitter struct {
	baseline  string
	candidate string
	percent   int32 // 0–100，原子读写
}

func NewTrafficSplitter(baseline, candidate string) *TrafficSplitter {
	return &TrafficSplitter{baseline: baseline, candidate: candidate}
}

func (ts *TrafficSplitter) Route(sessionID string) string {
	p := atomic.LoadInt32(&ts.percent)
	if p <= 0 {
		return ts.baseline
	}
	if p >= 100 {
		return ts.candidate
	}
	if int(fnvHash(sessionID)%100) < int(p) {
		return ts.candidate
	}
	return ts.baseline
}

func (ts *TrafficSplitter) SetPercent(p int32) {
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	atomic.StoreInt32(&ts.percent, p)
}

func (ts *TrafficSplitter) Rollback() {
	atomic.StoreInt32(&ts.percent, 0)
}

func fnvHash(s string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}
