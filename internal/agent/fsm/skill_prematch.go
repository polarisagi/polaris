package fsm

import (
	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/types"
)

// skillPreMatch 是 System-1 短路所需的技能匹配结果（B-1：在 FSM 锁外算好再带进锁）。
type skillPreMatch struct {
	applied bool // 本轮是否真的执行了匹配（surpriseIndex 未超阈值且 matcher 已注入）
	skillID string
	score   float64
	err     error
}

// preMatchSkill 在**不持 sm.mu** 的前提下完成技能语义检索（含 IO）。
//
// 第二个返回值表示「本次 trigger 是否需要覆写 sCtx 的 pre-match 字段」——
// 对 TriggerIntentReceived 恒为 true（即使没匹配也要把上一轮的残值清掉），
// 其余 trigger 为 false（不碰这组字段）。
//
// 为什么不能在锁内做：MatchIntent 走语义检索，耗时不可控，锁内调用会让所有并发
// Dispatch 排队，违反 HE-5（FSM 锁内禁止 IO）。同类缺陷此前已修过一次
// （GR-4-003 的 dispatchReplanExtensionActivation），B-1 是它在另一条路径上的复发。
func (sm *StateMachine) preMatchSkill(sCtx *StateContext, trigger types.AgentTrigger) (skillPreMatch, bool) {
	if trigger != types.TriggerIntentReceived {
		return skillPreMatch{}, false
	}
	if sm.skillMatcher == nil {
		return skillPreMatch{}, true
	}
	if metrics.GlobalSurpriseIndex().Current() >= system1BypassSurpriseCeiling {
		// 超阈值：本轮不做 System-1 短路。仍返回 true，让调用方把上一轮的
		// pre-match 残值清掉——否则阈值门等于形同虚设。
		return skillPreMatch{}, true
	}
	pm := skillPreMatch{applied: true}
	pm.skillID, pm.score, pm.err = sm.skillMatcher.MatchIntent(sCtx.RawIntentTS.UnsafeContent())
	return pm, true
}
