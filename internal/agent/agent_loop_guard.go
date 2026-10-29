package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/internal/agent/loopguard"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// throttleGraceRounds 是 state.yaml kill_switch.Stage1 "max_steps=3" 在 agentic 下的解释：
// 进入 Throttle 后再给最多 3 轮工具调用，随后注入收尾提示，仍不收尾才 abort。
// 不能照搬为"累计 FSM 步数 ≤3"——Throttle 到来时 StepsUsed 早已超过 3，会把回合当场截断，
// 模型来不及沉淀进度（2026-10-28 复核，ADR-0115 C2）。
const throttleGraceRounds = 3

// composeLoopWarningLocked 把各来源的提示合并写回 LoopWarningPrompt（易变上下文末尾，不进历史前缀，ADR-0115 T1/T2）。调用方持有 sCtx.Mu。
func (a *Agent) composeLoopWarningLocked() {
	var parts []string
	if a.sCtx.LoopBudgetWarning != "" {
		parts = append(parts, a.sCtx.LoopBudgetWarning)
	}
	if a.sCtx.LoopRepeatHint != "" {
		parts = append(parts, a.sCtx.LoopRepeatHint)
	}
	a.sCtx.LoopWarningPrompt = strings.Join(parts, "\n")
}

// syncThrottleLocked 根据 KillSwitch 当前阶段维护本回合的 Throttle 起点，返回是否处于 Throttle 及已持续轮数。调用方持有 sCtx.Mu。
// Throttle 自动回落后撤销由它注入的收尾提示，避免模型在已恢复的回合里被迫草草收尾。
func (a *Agent) syncThrottleLocked(currentRound int) (bool, int) {
	throttled := metrics.GlobalKillswitchStage.Load() == int32(types.KillThrottle)
	switch {
	case throttled && !a.sCtx.LoopThrottleActive:
		a.sCtx.LoopThrottleActive = true
		a.sCtx.LoopThrottleStartRound = currentRound
	case !throttled && a.sCtx.LoopThrottleActive:
		a.sCtx.LoopThrottleActive = false
		if a.sCtx.LoopThrottleWarned {
			a.sCtx.LoopThrottleWarned = false
			a.sCtx.LoopWarningInjected = false
			a.sCtx.LoopBudgetWarning = ""
			a.composeLoopWarningLocked()
		}
	}
	if !throttled {
		return false, 0
	}
	return true, currentRound - a.sCtx.LoopThrottleStartRound
}

// repeatHint 无进展提醒文本。措辞不假设模型出错：轮询等待也会得到相同结果，由模型自己判断。
func repeatHint(f loopguard.Finding) string {
	return fmt.Sprintf("无进展提醒：工具 %s 以相同参数已得到相同结果 %d 次。若不是在等待外部状态变化，请换一种方式，或基于现有信息给出阶段性结论并说明卡在哪里；若确实在等待，请拉长间隔。", f.Tool, f.Count)
}

// observeLoopRepeats 在一批工具执行完成后做无进展检测（ADR-0020 复核）。
// 首次越过提醒线：在易变上下文末尾提醒；越过收尾线：abort 并保存进度（与预算熔断同一出口）。
// 模型改道（本批没有任何调用仍处于提醒线之上）时撤销提醒。
func (a *Agent) observeLoopRepeats(ctx context.Context, calls []types.InferToolCall) error {
	th := config.CurrentThresholds().M4Kernel
	if th.LoopRepeatWarnAt <= 0 || len(calls) == 0 {
		return nil
	}

	results := a.collectToolResults(calls)

	a.sCtx.Mu.Lock()
	if a.sCtx.LoopRepeat == nil {
		a.sCtx.LoopRepeat = loopguard.New(th.LoopRepeatWarnAt, th.LoopRepeatStopAt)
	}
	det := a.sCtx.LoopRepeat
	a.sCtx.Mu.Unlock()

	var worst loopguard.Finding
	for _, c := range calls {
		res, ok := results[c.ID]
		if !ok {
			continue
		}
		f := det.Observe(c.Name, c.Input, res)
		if f.Level > worst.Level || (f.Level == worst.Level && f.Count > worst.Count) {
			worst = f
		}
	}

	if worst.Level == loopguard.Stop {
		metrics.RecordLoopTermination(ctx, "no_progress")
		err := apperr.New(apperr.CodeResourceExhausted,
			fmt.Sprintf("no-progress loop: tool %s returned identical result %d times", worst.Tool, worst.Count))
		a.abortTurn(ctx, err)
		return err
	}

	a.sCtx.Mu.Lock()
	if worst.Level == loopguard.Warn {
		if a.sCtx.LoopRepeatHint == "" {
			metrics.RecordLoopTermination(ctx, "no_progress_warned")
		}
		a.sCtx.LoopRepeatHint = repeatHint(worst)
	} else {
		a.sCtx.LoopRepeatHint = ""
	}
	a.composeLoopWarningLocked()
	a.sCtx.Mu.Unlock()
	return nil
}

// collectToolResults 从刚追加的消息尾部取出本批调用的 tool_result 文本（按 tool_use_id 索引）。
// 直接读消息而不是改执行路径：执行函数不属于本模块的熔断职责，且消息是模型实际看到的结果。
func (a *Agent) collectToolResults(calls []types.InferToolCall) map[string]string {
	want := make(map[string]struct{}, len(calls))
	for _, c := range calls {
		want[c.ID] = struct{}{}
	}
	out := make(map[string]string, len(calls))

	a.sCtx.Mu.RLock()
	defer a.sCtx.Mu.RUnlock()
	msgs := a.sCtx.LoopMessages
	for i := len(msgs) - 1; i >= 0 && len(out) < len(want) && i >= len(msgs)-(4*len(calls)+8); i-- {
		for _, p := range msgs[i].Parts {
			m, ok := p.(map[string]any)
			if !ok || m["type"] != "tool_result" {
				continue
			}
			id, _ := m["tool_use_id"].(string)
			if _, hit := want[id]; !hit {
				continue
			}
			if _, dup := out[id]; !dup {
				out[id] = msgs[i].Content
			}
		}
	}
	return out
}
