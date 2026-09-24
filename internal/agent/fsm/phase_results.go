package fsm

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/internal/agent/schemavalidate"
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
	"github.com/polarisagi/polaris/pkg/util"
)

// 回合路由标签（metrics.RecordTurnRoute），ADR-0098。
const (
	routeDirect           = "direct"
	routePlan             = "plan"
	routePlanEmpty        = "plan_empty"
	routePerceiveUnparsed = "perceive_unparsed"
	routeContinue         = "reflect_continue"
	routeReplanExhausted  = "replan_exhausted_reply"
)

// applyPerceiveResult 把 Perceive 输出解析进 TaskModel 并决定路由（ADR-0098 决策三）。
//
// 路由由 Go 在结构化解析、Schema 校验之后决定，LLM 只提供字段值（HE-5）。
// 解析失败不判回合失败：保留 SetTaskIntent 写入的原始意图作 Goal，保守走 S_PLAN，
// 完整管线在 Plan 得出空 DAG 时仍会转直答——感知失败不应让用户拿不到回复。
func (sm *StateMachine) applyPerceiveResult(sCtx *StateContext, fill []byte) (types.State, error) {
	if state, err := sm.onPerceiveSuccess(protocol.StateContext{}, fill); err != nil {
		return state, err
	}
	raw := []byte(util.ExtractJSONBraces(string(fill)))

	var tm TaskModel
	reason := schemavalidate.Validate("perceive_task", raw)
	if reason == nil {
		reason = json.Unmarshal(raw, &tm)
	}
	if reason == nil && strings.TrimSpace(tm.Goal) == "" {
		reason = apperr.New(apperr.CodeInvalidInput, "perceive: empty Goal")
	}
	if reason != nil {
		slog.Warn("perceive: TaskModel unparsable, routing to S_PLAN conservatively", "err", reason, "fill_bytes", len(fill))
		metrics.RecordTurnRoute(context.Background(), routePerceiveUnparsed)
		return "S_PERCEIVE_DONE", nil
	}

	sCtx.Mu.Lock()
	sCtx.TaskModel = &tm
	sCtx.Mu.Unlock()

	if tm.NeedsTools != nil && !*tm.NeedsTools {
		metrics.RecordTurnRoute(context.Background(), routeDirect)
		return "S_PERCEIVE_DIRECT", nil
	}
	metrics.RecordTurnRoute(context.Background(), routePlan)
	return "S_PERCEIVE_DONE", nil
}

// applyReflectResult 在既有 onReflectSuccess（learning 落盘）之外，把反思结论留给
// S_RESPOND：回复需据此如实报告"目标是否达成"，否则执行失败的回合会被写成成功。
func (sm *StateMachine) applyReflectResult(sCtx *StateContext, pCtx protocol.StateContext, fill []byte) (types.State, error) {
	raw := []byte(util.ExtractJSONBraces(string(fill)))
	var ref ReflectionModel
	if err := json.Unmarshal(raw, &ref); err == nil {
		sCtx.Mu.Lock()
		sCtx.Reflection = &ref
		sCtx.Mu.Unlock()
	}
	state, err := sm.onReflectSuccess(pCtx, fill)
	if err != nil || state != "S_REFLECT_DONE" {
		return state, err
	}
	if sm.shouldContinue(sCtx, raw) {
		return "S_REFLECT_CONTINUE", nil
	}
	return state, nil
}

// shouldContinue 观察—再规划（ADR-0098 决策八）：反思**显式**判定目标未达成且重规划
// 预算尚余时回到规划。字段缺失/解析失败按已达成处理——宁可少跑一轮，不因输出不
// 规范空转。只在 replanCount+1 < MaxReplan 时继续，不把回合推进到耗尽分支。
func (sm *StateMachine) shouldContinue(sCtx *StateContext, raw []byte) bool {
	var probe struct {
		GoalAchieved *bool
		Errors       []string
	}
	if json.Unmarshal(raw, &probe) != nil || probe.GoalAchieved == nil || *probe.GoalAchieved {
		return false
	}
	if sm.ReplanCount()+1 >= sCtx.MaxReplan {
		return false
	}
	reason := "previous round did not achieve the goal"
	if len(probe.Errors) > 0 {
		reason += ": " + strings.Join(probe.Errors, "; ")
	}
	sCtx.RecordReplanFeedback(reason)
	metrics.RecordTurnRoute(context.Background(), routeContinue)
	return true
}

// onRespondSuccess 空回复不得静默当作完成（A-01）：用户会看到一轮什么都没有的回答。
func onRespondSuccess(_ protocol.StateContext, fill []byte) (types.State, error) {
	if len(bytes.TrimSpace(fill)) == 0 {
		return "S_RESPOND_FAILED", apperr.New(apperr.CodeInternal, "respond: LLM returned empty reply")
	}
	return "S_RESPOND_DONE", nil
}

func onRespondFailure(_ protocol.StateContext, err error) (types.State, error) {
	return "S_RESPOND_FAILED", apperr.Wrap(apperr.CodeInternal, "respond: LLM fill failed", err)
}
