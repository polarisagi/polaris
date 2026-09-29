package types

import "testing"

// 内核阶段常量必须与 TurnPhase 取值一致：llmPurposeOf 直接把 TurnPhase 当 purpose 写入 llm_calls，
// 两处漂移会让「内核阶段硬排除缓存」对真实的 perceive/plan 调用失效。
func TestKernelPurposesMatchTurnPhase(t *testing.T) {
	pairs := map[TurnPhase]string{
		TurnPhasePerceive: PurposePerceive,
		TurnPhasePlan:     PurposePlan,
		TurnPhaseExecute:  PurposeExecute,
		TurnPhaseReflect:  PurposeReflect,
		TurnPhaseRespond:  PurposeRespond,
	}
	for phase, purpose := range pairs {
		if string(phase) != purpose {
			t.Errorf("TurnPhase %q 与 purpose 常量 %q 不一致", phase, purpose)
		}
		if !IsKernelPurpose(string(phase)) {
			t.Errorf("内核阶段 %q 必须被 IsKernelPurpose 识别", phase)
		}
	}
}

func TestIsKernelPurpose(t *testing.T) {
	kernel := []string{"perceive", "plan", "execute", "reflect", "respond", "validate", "validate_watchdog",
		"kernel", "plan_prm_candidate", "unspecified", ""}
	for _, p := range kernel {
		if !IsKernelPurpose(p) {
			t.Errorf("%q 应判为内核用途", p)
		}
	}
	background := []string{PurposeGraphRAGExtract, PurposeGraphRAGSummary, PurposeGraphRAGCommunity,
		PurposeGraphRAGConcept, PurposeRAGSummaryTree, PurposeRAGQueryRewrite, PurposeMemoryWriteFilter}
	for _, p := range background {
		if IsKernelPurpose(p) {
			t.Errorf("%q 是确定性后台用途，不应判为内核", p)
		}
	}
}
