package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

// Run 启动 Agent 事件循环（Suspend-on-Idle）。
// 空闲时阻塞在 intent channel 上，不轮询——符合 par_inv_05。
//
//nolint:gocyclo
func (a *Agent) Run(ctx context.Context) error {
	select {
	case <-a.done:
		// 上次回合 Run 已退出，本次重新 Run 时重置 done 通道与 doneOnce
		a.done = make(chan struct{})
		a.doneOnce = sync.Once{}
	default:
	}

	// [GD-13-006] Run() 循环退出（无论正常终态、超步熔断还是 ctx 取消）时关闭
	// done，通知 Pool.release() 内核已真正停止，可以安全归还容量令牌。
	// doneOnce 防止理论上的重复调用导致 close 已关闭 channel 而 panic。
	defer a.doneOnce.Do(func() { close(a.done) })

	// [M04 §8 崩溃检测] 标记本会话当前有一个 Run() 生命周期在处理中；
	// 无论后续走哪条退出路径都会清除（含正常终态/超步熔断/ctx 取消）。
	// 若进程在两者之间崩溃，标记残留，供 boot 阶段崩溃恢复驱动器识别。
	a.markInFlight(ctx)
	defer a.clearInFlight()

	// polaris.agents.active：以 Run() 生命周期计数（GR-1.2-003，此前全仓无写入方，指标恒 0）。
	metrics.ActiveAgentsCount.Add(1)
	defer metrics.ActiveAgentsCount.Add(-1)

	// 从 AgentConfig 初始化步骤预算（仅在首次 Run 时设置，支持外部注入覆盖）
	if a.Config.MaxSteps > 0 && a.sCtx.MaxStepsLimit == 0 {
		a.sCtx.MaxStepsLimit = a.Config.MaxSteps
		a.sCtx.InitialMaxStepsLimit = a.Config.MaxSteps
	}
	idleTimeout := a.Config.IdleTimeoutSec
	if idleTimeout <= 0 {
		idleTimeout = 300
	}
	idleTimer := time.NewTimer(time.Duration(idleTimeout) * time.Second)
	defer idleTimer.Stop()

	for {
		// 动态加载已安装插件信息 (仅当无 effect 运行时安全刷新，避免与 executeEffect 并发竞争)
		if !a.effectRunning.Load() {
			a.refreshInstalledExtensions(ctx)
			// GD-14-005：工作区上下文与扩展清单同周期刷新，共用同一个
			// "无 effect 运行"的安全窗口。
			a.refreshWorkspaceContext(ctx)
		}

		select {
		case trigger := <-a.intent:
			idleTimer.Reset(time.Duration(idleTimeout) * time.Second)
			// ADR-0097 决策三补：headless 子 Agent 的命名空间在 SendIntent 之前才注入，
			// 循环顶部那次刷新可能早于注入。新意图到达时补刷一次，使首个 effect
			// 即带上正确的项目作用域（项目 ID / 工作目录 / 项目指令）。
			if trigger == types.TriggerIntentReceived && !a.effectRunning.Load() {
				a.refreshWorkspaceContext(ctx)
			}
			// Adaptive Max-Steps: 步骤计数 + 预算熔断
			a.sCtx.Mu.Lock()
			a.sCtx.StepsUsed++
			limit := a.sCtx.MaxStepsLimit
			used := a.sCtx.StepsUsed
			a.sCtx.Mu.Unlock()

			if limit > 0 && used > limit {
				// 经统一出口收尾：此前 ForceState 后直接返回，订阅方等不到 task_done（ADR-0098 决策七）。
				stepErr := apperr.New(apperr.CodeInternal,
					fmt.Sprintf("MAX_STEPS_EXCEEDED: steps %d > limit %d", used, limit))
				a.abortTurn(ctx, stepErr)
				return stepErr
			}

			// GR-4-004: 消费 pendingRedirectCh——如果有 InterruptRedirect 请求在途，
			// 在主循环单线程内安全地写入 sCtx.RawIntentTS（避免外部 goroutine 直接写的数据竞争）。
			if trigger == types.TriggerInterruptReceived {
				select {
				case redirect := <-a.pendingRedirectCh:
					if redirect != "" {
						level := types.TaintHigh
						if a.pendingRedirectOwner.Load() {
							level = types.TaintLow // 已认证的本地 owner 本人改写意图（ADR-0007 修订）
						}
						a.sCtx.Mu.Lock()
						a.sCtx.RawIntentTS = taint.NewTaintedString(
							redirect,
							taint.TaintSource{OriginTaintLevel: level},
							"user_interrupt_redirect",
						)
						a.sCtx.Mu.Unlock()
					}
				default:
				}
			}

			fromState := a.sm.Current()
			effects, err := a.sm.Dispatch(ctx, a.sCtx, trigger)
			// [HE-1] FSM 推进是交互式回合的主控制流，此前全程无埋点：一旦回合
			// 空转（不产 token 也不报错），日志里没有任何线索可区分"没触发状态
			// 转移""转移了但没产生 Effect""Effect 产出了但没调 LLM"。回合级日志
			// 量极小（每轮 1~N 条），值得常驻。
			slog.InfoContext(ctx, "kernel: fsm dispatched",
				"agent_id", a.ID, "session", a.sCtx.SessionID, "trigger", trigger,
				"from", fromState, "to", a.sm.Current(), "effects", len(effects), "err", err)
			if err != nil {
				// context 取消由 M8 Reaper 触发——直接退出，不触发 S_ROLLBACK
				if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, fsm.ErrReplanExhausted) {
					return ctxErr //nolint:wrapcheck // 保留 context 哨兵身份，供调用方 errors.Is/== 判断
				}
				// ErrReplanExhausted：sm.Dispatch 内部已转 S_FAILED；其余为转移表缺口。
				// 两者都必须经统一出口收尾，否则订阅方既收不到原因也等不到 task_done。
				a.abortTurn(ctx, err)
				return apperr.Wrap(apperr.CodeInternal, "Agent.Run", err)
			}

			// 执行 Effects: LLMFillEffect → 调 LLM；DeterministicEffect → 直接执行
			for _, effect := range effects {
				// 串行保证：上一个 effect 未完成时等待其释放。等待期间必须同时消费 effectDone
				// （GR-4.1-001）：effectDone 容量 1，若其中压着一个未读结果，正在运行的 effect
				// 会阻塞在发送上、永远走不到释放 effectRunning 的 defer，主循环在此死等。
				for !a.effectRunning.CompareAndSwap(false, true) {
					select {
					case <-ctx.Done():
						return ctx.Err() //nolint:wrapcheck // 保留 context 哨兵身份
					case result := <-a.effectDone:
						if done, err := a.handleEffectResult(ctx, result); done || err != nil {
							return err
						}
					case <-a.effectIdle:
					}
				}
				concurrent.SafeGo(ctx, "agent.executeEffect", func(execCtx context.Context) {
					// panic 由 SafeGo 外层 recover（打日志+计量），goroutine 静默退出。
					// 此处 defer 保证无论正常返回还是 panic，effectRunning 都能解锁并发出空闲信号，
					// 避免主循环在 effectDone select 上永久阻塞。
					defer func() {
						a.effectRunning.Store(false)
						select {
						case a.effectIdle <- struct{}{}:
						default:
						}
					}()
					result := a.executeEffect(execCtx, effect)
					select {
					case a.effectDone <- result:
					case <-execCtx.Done():
						if result.Err != nil {
							slog.WarnContext(execCtx, "agent: effect result dropped on ctx cancel", "err", result.Err)
						}
					}
				})
			}

			// 终态检查
			current := a.sm.Current()
			if current == types.AgentStateComplete || current == types.AgentStateFailed {
				slog.InfoContext(ctx, "kernel: terminal state reached",
					"agent_id", a.ID, "session", a.sCtx.SessionID, "state", current)
				a.handleTerminalState(ctx, current)
				return nil
			}

		case result := <-a.effectDone:
			idleTimer.Reset(time.Duration(idleTimeout) * time.Second) // effect 结束也是活动，不应在紧随其后被判空闲
			if result.Err != nil {
				// [HE-1] Effect 错误此前只经 Run() 返回值上抛，既不进流、也只在
				// pool.go 那层打一条不含错误来源阶段的 warn。
				slog.WarnContext(ctx, "kernel: effect failed",
					"agent_id", a.ID, "session", a.sCtx.SessionID,
					"state", a.sm.Current(), "err", result.Err)
			}
			if done, err := a.handleEffectResult(ctx, result); done || err != nil {
				return err
			}

		case <-idleTimer.C:
			if a.sm.Current() == types.AgentStateSuspended {
				// 已经在 Suspended 状态，静默等待意图唤醒
				continue
			}
			// 只有真正空闲（S_IDLE 且无 effect 在跑）才允许挂起。回合内的长耗时工具、等待人工审批
			// （无人值守审批可长达 24 小时）都会超过空闲超时；此前计时器在非 Idle 态仍会 ForceState 成
			// S_SUSPENDED，工具返回后的 TriggerLoopExecDone 便报 "no transition from 11 with trigger 24"。
			if a.sm.Current() != types.AgentStateIdle || a.effectRunning.Load() {
				idleTimer.Reset(time.Duration(idleTimeout) * time.Second)
				continue
			}
			// Suspend-on-Idle：持久化状态后退出，由上层 Supervisor 决定是否重启
			if _, err := a.sm.Dispatch(ctx, a.sCtx, types.TriggerSuspend); err != nil {
				slog.Warn("kernel: suspend transition failed", "err", err)
				a.sm.ForceState(types.AgentStateSuspended) // fallback
			}
			continue
		case <-ctx.Done():
			return ctx.Err() //nolint:wrapcheck // 保留 context 哨兵身份，供调用方 errors.Is/== 判断
		}
	}
}
