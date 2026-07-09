package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
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
// maxSize：由 TierParameters.MaxAgents 传入。
// 实际值来自 configs/defaults.toml（可部署覆盖）：
// Tier-0 服务器 HT0 = 3，Tier-0 桌面 HT0 = 2，Tier-1 = 16。
// 权威阈值见 docs/arch/spec/state.yaml §thresholds.max_agents_*_ht0。
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

// AcquireHeadless 供 Cron/Workflow/Webhook 等非交互式触发方注入 Intent 并同步获取最终结果，
// 内部完整复用 Agent Kernel 的 FSM/DAG/安全 Gate/Reflection/Replan 能力。
func (p *Pool) AcquireHeadless(ctx context.Context, intent types.Intent, opts ...types.HeadlessOption) (*types.AgentResult, error) {
	sessionID := "headless-" + time.Now().Format("20060102150405") + "-" + fmt.Sprintf("%x", time.Now().UnixNano())
	agent, release, err := p.Acquire(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	defer release()

	opt := &types.HeadlessOptions{}
	for _, o := range opts {
		o(opt)
	}

	intentBytes, _ := json.Marshal(intent)
	agent.SetTaskIntent(intentBytes)

	stream := agent.SubscribeStream(ctx)
	if err := agent.SendIntent(types.TriggerIntentReceived); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to send intent", err)
	}

	start := time.Now()
	var finalOutput string
	for ev := range stream {
		if ev.Type == types.AgentStreamEventError {
			return nil, apperr.New(apperr.CodeInternal, ev.Content)
		}
		if ev.Type == types.AgentStreamEventToken {
			finalOutput += ev.Content
		}
	}

	return &types.AgentResult{
		Output:    finalOutput,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
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
