package web_fetch

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// maxTrackedHosts 触发过期条目清扫的 host 数量阈值。
const maxTrackedHosts = 1024

// HostLimiter 同主机请求限速器，防止爬取过频（ADR-0114 决策三）。
type HostLimiter struct {
	mu          sync.Mutex
	last        map[string]time.Time
	minInterval func() time.Duration
}

// NewHostLimiter 创建 HostLimiter。
func NewHostLimiter(minInterval func() time.Duration) *HostLimiter {
	return &HostLimiter{
		last:        make(map[string]time.Time),
		minInterval: minInterval,
	}
}

// Wait 等待直至距离该 host 上次访问达到最小间隔；若 ctx 取消则提前返回。
func (l *HostLimiter) Wait(ctx context.Context, host string) error {
	if l == nil || l.minInterval == nil {
		return nil
	}
	interval := l.minInterval()
	if interval <= 0 {
		return nil
	}

	h := host
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		h = parsedHost
	}
	h = strings.ToLower(strings.TrimSpace(h))
	if h == "" {
		return nil
	}

	l.mu.Lock()
	lastTime, ok := l.last[h]
	now := time.Now()
	var waitDur time.Duration
	if ok {
		elapsed := now.Sub(lastTime)
		if elapsed < interval {
			waitDur = interval - elapsed
		}
	}
	// 长期运行的定时采集会不断遇到新 host；超过 maxTrackedHosts 时清掉已过间隔的
	// 条目（它们对限速已无影响），摊销 O(n) 可接受。
	if len(l.last) > maxTrackedHosts {
		for k, t := range l.last {
			if now.Sub(t) > interval {
				delete(l.last, k)
			}
		}
	}
	// 预先占位本次访问的预计完成时点
	l.last[h] = now.Add(waitDur)
	l.mu.Unlock()

	if waitDur > 0 {
		t := time.NewTimer(waitDur)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return apperr.Wrap(apperr.CodeCancelled, "host limiter canceled", ctx.Err())
		}
	}
	return nil
}
