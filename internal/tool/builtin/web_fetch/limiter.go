package web_fetch

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

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
