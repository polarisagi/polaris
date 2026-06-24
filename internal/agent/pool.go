package agent

import (
	"context"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// Pool 管理 per-session Agent 实例，容量受 maxSize 限制。
// 超容量时 Acquire 等待至多 acquireTimeout 后返回 CodeResourceExhausted。
// idle 超过 idleTimeout 的 Agent 实例被自动回收（Close 调用）。
type Pool struct {
	factory        func(sessionID string) *Agent
	maxSize        int
	acquireTimeout time.Duration
	idleTimeout    time.Duration

	mu       sync.Mutex
	sessions map[string]*poolEntry
	sem      chan struct{}
}

type poolEntry struct {
	agent    *Agent
	lastUsed time.Time
	refs     int
}

// NewPool 构造 AgentPool。
// factory 每次为新 sessionID 创建一个完整配置的 Agent（含 SurpriseCalc 等注入）。
// maxSize：Tier-0 传 4，Tier-1 传 16。
func NewPool(factory func(sessionID string) *Agent, maxSize int) *Pool {
	p := &Pool{
		factory:        factory,
		maxSize:        maxSize,
		acquireTimeout: 100 * time.Millisecond,
		idleTimeout:    10 * time.Minute,
		sessions:       make(map[string]*poolEntry),
		sem:            make(chan struct{}, maxSize),
	}
	// 填满 sem（可用令牌）
	for range maxSize {
		p.sem <- struct{}{}
	}
	return p
}

// Acquire 返回 sessionID 对应的 Agent 及释放回调。
func (p *Pool) Acquire(ctx context.Context, sessionID string) (protocol.AgentController, func(), error) {
	// 等待容量令牌
	acquireCtx, cancel := context.WithTimeout(ctx, p.acquireTimeout)
	defer cancel()
	select {
	case <-p.sem:
	case <-acquireCtx.Done():
		return nil, nil, apperr.New(apperr.CodeResourceExhausted, "agent pool: capacity exhausted")
	}

	p.mu.Lock()
	entry, ok := p.sessions[sessionID]
	if !ok {
		entry = &poolEntry{agent: p.factory(sessionID)}
		p.sessions[sessionID] = entry
	}
	entry.refs++
	entry.lastUsed = time.Now()
	agent := entry.agent
	p.mu.Unlock()

	release := func() {
		p.mu.Lock()
		entry.refs--
		entry.lastUsed = time.Now()
		p.mu.Unlock()
		p.sem <- struct{}{} // 归还令牌
	}
	return agent, release, nil
}

// GC 清理 idle 超时的 session，应由外部低频 ticker 调用（如每 5 分钟）。
func (p *Pool) GC() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for id, entry := range p.sessions {
		if entry.refs == 0 && now.Sub(entry.lastUsed) > p.idleTimeout {
			delete(p.sessions, id)
		}
	}
}
