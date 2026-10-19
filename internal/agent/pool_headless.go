package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/security/guard"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// headlessPromptGuard 供 AcquireHeadless 扫描 finalOutput 出站防护。
var headlessPromptGuard = sync.OnceValue(func() *guard.SystemPromptGuard { //nolint:gochecknoglobals // sync.OnceValue 懒加载单例，SystemPromptGuard 内部自带锁，无外部可变状态
	g := guard.NewSystemPromptGuard(0)
	for _, frag := range guard.KernelPromptFragments() {
		g.AddFragment(frag)
	}
	return g
})

// AcquireHeadless 供 Cron/Workflow/Webhook 等非交互式触发方注入 Intent 并同步获取最终结果，
// 内部完整复用 Agent Kernel 的 FSM/DAG/安全 Gate/Reflection/Replan 能力。
func (p *Pool) AcquireHeadless(ctx context.Context, intent types.Intent, opts ...types.HeadlessOption) (*types.AgentResult, error) {
	opt := &types.HeadlessOptions{}
	for _, o := range opts {
		o(opt)
	}

	// GD-13-001：优先复用调用方透传的真实业务 SessionID
	sessionID := opt.SessionID
	if sessionID == "" {
		sessionID = "headless-" + time.Now().Format("20060102150405") + "-" + fmt.Sprintf("%x", time.Now().UnixNano())
	}
	// headless 任务使用 ctx 自身超时（最长 10 分钟），后台任务排队而非立即失败
	agent, release, err := p.acquireInner(ctx, sessionID, true)
	if err != nil {
		return nil, err
	}
	defer release()

	// ADR-0084：透传委派链深度
	agent.SetSpawnDepth(opt.SpawnDepth)

	// GD-14-001：透传协同任务共享记忆命名空间
	agent.SetMemoryNamespace(opt.Namespace)
	// ADR-0103 决策三：子 Agent 角色
	agent.SetAgentProfile(opt.Profile)
	if opt.Profile != nil {
		metrics.GlobalSubagentRunsTotal.Add(1)
	}

	// R3-07 / WP2：透传显式完成模式
	if opt.LoopCompletion != "" {
		agent.SetLoopCompletion(opt.LoopCompletion)
	}

	intentBytes, _ := json.Marshal(intent)
	agent.SetTaskIntent(taint.NewTaintedString(string(intentBytes), taint.TaintSource{OriginTaintLevel: types.TaintHigh}, "sys"))

	stream := agent.SubscribeStream(ctx)
	if err := agent.SendIntent(types.TriggerIntentReceived); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "failed to send intent", err)
	}

	start := time.Now()
	var finalOutput string
	for ev := range stream {
		// GD-13-001：若注入了事件回调（子 Agent 事件透传），先转发给回调方。
		if opt.EventCallback != nil {
			opt.EventCallback(ev)
		}
		if ev.Type == types.AgentStreamEventError {
			if p.killGate != nil {
				p.killGate.ReportError()
			}
			return nil, apperr.New(apperr.CodeInternal, ev.Content)
		}
		if ev.Type == types.AgentStreamEventToken {
			finalOutput += ev.Content
		}
		// 收到 task_done 后提前跳出循环
		if ev.Type == types.AgentStreamEventStatus && ev.Content == "task_done" {
			break
		}
	}

	// [W-2-B] SystemPromptGuard：headless 路径一次性扫描完整 finalOutput
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
