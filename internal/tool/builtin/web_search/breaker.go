package web_search

import (
	"context"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

// DDG 的 HTML 端点遇到反爬时返回 202 挑战页。同一会话里连续多次撞上，说明这个出口 IP 已被盯上：
// 继续每次都先请求 DDG（再加 2 秒退避重试）只是白白拖慢每一次搜索。熔断器在连续 breakerThreshold 次
// 限流后让该会话在 breakerOpenFor 内直接跳过 DDG、走降级链；到期后放行一次探测，成功即复位。
const (
	breakerThreshold = 2
	breakerOpenFor   = 5 * time.Minute
	breakerMaxKeys   = 256 // 会话键上限，防止长期运行下无界增长
)

type breakerState struct {
	consecutive int
	openUntil   time.Time
	touched     time.Time
}

type rateBreaker struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[string]*breakerState
}

func newRateBreaker() *rateBreaker {
	return &rateBreaker{now: time.Now, entries: map[string]*breakerState{}}
}

// ddgBreaker 进程内共享；键为会话 ID（取自 ctx 的 CtxSessionIDKey，无会话时用空串，即全局一档）。
var ddgBreaker = newRateBreaker() //nolint:gochecknoglobals // 进程级限流状态

func breakerKey(ctx context.Context) string {
	k, _ := ctx.Value(protocol.CtxSessionIDKey{}).(string)
	return k
}

// allow 报告是否可以请求 DDG；熔断期内返回 (false, 剩余时间)。nil 接收者恒放行。
func (b *rateBreaker) allow(key string) (bool, time.Duration) {
	if b == nil {
		return true, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.entries[key]
	if st == nil {
		return true, 0
	}
	if left := st.openUntil.Sub(b.now()); left > 0 {
		return false, left
	}
	return true, 0
}

// record 记录一次搜索的结果：限流累加并在达阈值时开路；成功复位；其它错误不改变状态。
func (b *rateBreaker) record(key string, limited, ok bool) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	st := b.entries[key]
	switch {
	case ok:
		delete(b.entries, key)
	case limited:
		if st == nil {
			if len(b.entries) >= breakerMaxKeys {
				b.evictOldestLocked()
			}
			st = &breakerState{}
			b.entries[key] = st
		}
		st.consecutive++
		st.touched = now
		if st.consecutive >= breakerThreshold {
			st.openUntil = now.Add(breakerOpenFor)
			st.consecutive = 0 // 到期后的探测从零计起：再连续限流才会重新开路
		}
	}
}

func (b *rateBreaker) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for k, st := range b.entries {
		if oldestKey == "" || st.touched.Before(oldest) {
			oldestKey, oldest = k, st.touched
		}
	}
	delete(b.entries, oldestKey)
}
