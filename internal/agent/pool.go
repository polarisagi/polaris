package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// headlessPromptGuard 供 AcquireHeadless（Cron/Workflow/Webhook 触发路径的唯一
// 收敛入口）扫描 finalOutput，堵住 M11 §2.2 六阶段出站防护流水线此前完全未覆盖
// 的一段——SSE 交互路径（gateway/server/chat/sse.go）早已接入 SystemPromptGuard，
// 但 headless 路径此前从未调用，Cron/Workflow/Webhook 触发的响应对系统提示词
// 逐字泄露（OWASP LLM07）完全不设防。headless 场景不像 SSE 有逐会话的 M9 GEPA
// 激活提示词可注册，只注册内核阶段模板（guard.KernelPromptFragments，见该函数
// 注释）——这是"系统提示词"的静态主体，覆盖面已经是此前的从 0 到有。
// 用 sync.OnceValue 构造单例：SystemPromptGuard 本身线程安全（Scan/AddFragment
// 均有锁），跨请求复用避免每次 AcquireHeadless 都重新注册相同的静态片段。
var headlessPromptGuard = sync.OnceValue(func() *guard.SystemPromptGuard { //nolint:gochecknoglobals // sync.OnceValue 懒加载单例，SystemPromptGuard 内部自带锁，无外部可变状态
	g := guard.NewSystemPromptGuard(0)
	for _, frag := range guard.KernelPromptFragments() {
		g.AddFragment(frag)
	}
	return g
})

// KillSwitchGate agent 包对系统级三阶段熔断状态的消费端接口（HE-3：接口在调用方定义）。
// 实现：security.KillSwitch（通过 Pool.WithKillSwitchGate 注入，nil 时不做任何熔断检查，
// 行为与注入前完全一致，保证既有测试无需构造该依赖）。
//
// [2026-07-12 补上的真实缺口] 此前 KillSwitch 三阶段熔断（ADR-0009）仅在启动时
// 检查 .fullstop 文件、构造对象、更新 Prometheus gauge，其 CheckAndAct/ReportError/
// ReportSafetyViolation/IsSealed 等状态转移与查询方法在全仓库零调用——熔断升级到
// Pause/FullStop 后，运行中的进程不会拒绝任何新 Agent 执行请求，只有下次重启才会
// 被 .fullstop 文件挡住。AgentPool.Acquire/AcquireHeadless 是全部交互式与
// Cron/Workflow/Webhook headless 触发路径的唯一收敛入口（ADR-0039 Gateway 控制权
// 移交 FSM 之后），在此处做一次性检查即可覆盖全部路径，无需在各 handler 分别判断。
type KillSwitchGate interface {
	// Allowed 返回当前是否允许启动新的 Agent 执行；Pause/FullStop 阶段返回 false。
	Allowed() bool
	// ReportError 上报一次 Agent 内核异常退出，供三阶段熔断累计错误计数判定。
	ReportError()
}

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
	killGate KillSwitchGate
}

// WithKillSwitchGate 注入 KillSwitch 熔断门（可选，nil 安全）。
// 调用方：cmd/polaris/boot_agent.go，注入 sb.KS（*security.KillSwitch）。
func (p *Pool) WithKillSwitchGate(g KillSwitchGate) {
	p.mu.Lock()
	p.killGate = g
	p.mu.Unlock()
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

// releaseWaitTimeout release() 等待内核 Run() 循环真正退出的上限（GD-13-006）。
// 超时仍会归还容量令牌（避免异常场景下永久占用池容量），但会记录警告日志。
const releaseWaitTimeout = 3 * time.Second

// newPoolEntry 创建 sessionID 对应的全新 Agent 实例，并立即以其自身生命周期
// ctx（NewAgent 内部创建，M8 Reaper/Shutdown 复用同一 cancel 函数）启动常驻
// Run() 事件循环 goroutine。
//
// [2026-07-12 复核发现的真实缺口] 此前本文件仅调用 p.factory(sessionID) 构造
// Agent，从未在任何位置启动其 Run()——全仓库 grep 确认，除 Supervisor Tree
// 对单例 "agent-0" 的 sv.AddWorker("agent-0", func(ctx) error { return
// agent.Run(ctx) })（cmd/polaris/boot_agent.go）外，Pool 管理的 per-session
// Agent 从未有任何 goroutine 消费 a.intent。SendIntent 写入的是带缓冲 channel
// （cap=10），短期内不会报错超时，这掩盖了 FSM 实际从未推进状态的事实——
// AgentPool 交互式聊天路径（sse.go handleAgentStreamFSM）端到端从未被测试
// 覆盖过（internal/agent 无 pool_test.go，chat 包测试也不构造真实 AgentPool），
// 是本次复核发现的真实（而非假设）生产缺陷。现补上：每个新建 Agent 立即启动
// Run()，语义与 Supervisor 对 "agent-0" 的启动方式一致，只是宿主从 Supervisor
// Tree 换成 Pool 自身；对应地 GC() 需在回收 idle entry 时调用 Shutdown() 停止
// 该 goroutine（见下方 GC 注释），否则会从"FSM 从不运行"变成"goroutine 泄漏"。
func (p *Pool) newPoolEntry(sessionID string) *poolEntry {
	ag := p.factory(sessionID)
	concurrent.SafeGo(ag.ctx, "agent-pool.kernel."+sessionID, func(ctx context.Context) {
		if err := ag.Run(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("agent pool: kernel Run() exited with error", "session", sessionID, "err", err)
			if p.killGate != nil {
				p.killGate.ReportError()
			}
		}
	})
	return &poolEntry{agent: ag}
}

// Acquire 返回 sessionID 对应的 Agent 及释放回调。
func (p *Pool) Acquire(ctx context.Context, sessionID string) (protocol.AgentController, func(), error) {
	// KillSwitch 熔断检查：Pause/FullStop 阶段拒绝启动任何新 Agent 执行
	// （进行中的 Agent 不受影响，语义对齐 KillPause 注释"中止并保存进行中请求的状态"——
	// 由 KillSwitch 状态变迁时的上层编排负责，此处只负责"不再开新的"）。
	if p.killGate != nil && !p.killGate.Allowed() {
		return nil, nil, apperr.New(apperr.CodeInternal, "agent pool: system sealed by killswitch, rejecting new agent execution")
	}

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
	if ok && entry.refs == 0 {
		// [GD-13-005] 上一轮已到达终态（Complete/Failed）的 Agent，其 Run()
		// 循环已经返回退出，不再有任何 goroutine 消费 a.intent channel。
		// 若继续复用该实例，后续 SendIntent 会因无消费者而 50ms 超时，导致
		// 该 session 被永久锁定，直到 Pool.GC() 按 idleTimeout（默认 10 分钟）
		// 回收整个 entry 为止。此处提前判定并原地替换为全新 Agent 实例，
		// 语义上与"全新 session"完全等价，避免锁定窗口。
		curr := entry.agent.sm.Current()
		if curr == types.AgentStateComplete || curr == types.AgentStateFailed {
			entry = p.newPoolEntry(sessionID)
			p.sessions[sessionID] = entry
		}
	}
	if !ok {
		entry = p.newPoolEntry(sessionID)
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

		// [GD-13-002] 归还池前防御性中止：若任务未达终态，则静默下发 Abort，防止无感空跑
		curr := agent.sm.Current()
		if curr != types.AgentStateComplete && curr != types.AgentStateFailed {
			agent.Interrupt(types.InterruptRequest{Action: types.InterruptAbort})

			// [GD-13-006] Interrupt 只是异步投递中止信号（<200ms SLO），并不
			// 保证内核 Run() 循环已经处理完并真正退出、释放 Blackboard
			// Lease/SQLite 写锁等资源。此前 release() 在此处直接归还容量
			// 令牌，若客户端立即重连，Pool 会把仍在"濒死退出"过程中的
			// 同一 Agent 再次交给新请求，造成内部 channel/锁竞态。这里改为
			// 有界等待 Run() 真正退出（Done() 关闭）后再归还令牌；超时仍
			// 归还，避免极端场景下永久占用池容量，但记录警告以便排查。
			select {
			case <-agent.Done():
			case <-time.After(releaseWaitTimeout):
				slog.Warn("agent pool: release timed out waiting for kernel to stop",
					"session", sessionID, "timeout", releaseWaitTimeout)
			}
		}

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
			if p.killGate != nil {
				p.killGate.ReportError()
			}
			return nil, apperr.New(apperr.CodeInternal, ev.Content)
		}
		if ev.Type == types.AgentStreamEventToken {
			finalOutput += ev.Content
		}
	}

	// [W-2-B] SystemPromptGuard：headless 路径一次性扫描完整 finalOutput（不像
	// SSE 逐 chunk 扫描那样需要处理跨 chunk 边界问题——这里已经是拼接完成的
	// 全量文本），redact=true 净化后继续返回，不中断 Cron/Workflow/Webhook 自动化。
	if cleaned, scanErr := headlessPromptGuard().Scan(finalOutput, true); scanErr == nil {
		finalOutput = cleaned
	} else {
		slog.Warn("agent pool: system prompt guard scan failed on headless output", "session", sessionID, "err", scanErr)
	}

	return &types.AgentResult{
		Output:    finalOutput,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// GC 清理 idle 超时的 session，应由外部低频 ticker 调用（如每 5 分钟）。
//
// 2026-07-12 复核修复：newPoolEntry 补上 Run() 常驻 goroutine 后，仅从
// p.sessions 摘除 entry 不足以释放该 Agent——Run() 的 Suspend-on-Idle 循环
// 只有 Complete/Failed 终态或 ctx.Done() 才会返回，空闲挂起（Suspended）状态
// 会持续阻塞在 a.intent/idleTimer 上"不轮询"地等待。若不显式 Shutdown()，
// 这里的每一次回收都会变成一个永久泄漏的 goroutine（此前 Run() 从未启动，
// 该问题被同一个缺口意外掩盖了）。
func (p *Pool) GC() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for id, entry := range p.sessions {
		if entry.refs == 0 && now.Sub(entry.lastUsed) > p.idleTimeout {
			entry.agent.Shutdown()
			delete(p.sessions, id)
		}
	}
}
