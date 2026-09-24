package fsm

import (
	"errors"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestApplyPerceiveResult_Routing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fill      string
		wantState types.State
		wantGoal  string
	}{
		{"明确无需工具→直答", `{"Goal":"打招呼","NeedsTools":false}`, "S_PERCEIVE_DIRECT", "打招呼"},
		{"需要工具→规划", `{"Goal":"读 README","NeedsTools":true}`, "S_PERCEIVE_DONE", "读 README"},
		{"字段缺失→保守规划", `{"Goal":"做点什么"}`, "S_PERCEIVE_DONE", "做点什么"},
		{"围栏包裹仍可解析", "```json\n{\"Goal\":\"g\",\"NeedsTools\":false}\n```", "S_PERCEIVE_DIRECT", "g"},
		{"非 JSON→保守规划且不覆盖原 Goal", "我是 Polaris，你好", "S_PERCEIVE_DONE", "原始意图"},
		{"类型错误→保守规划", `{"Goal":"g","NeedsTools":"no"}`, "S_PERCEIVE_DONE", "原始意图"},
		{"Goal 为空→保守规划", `{"Goal":"  ","NeedsTools":false}`, "S_PERCEIVE_DONE", "原始意图"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sm := NewStateMachine(&dummyContextBuilder{})
			sCtx := &StateContext{TaskModel: &TaskModel{Goal: "原始意图"}}
			state, err := sm.applyPerceiveResult(sCtx, []byte(tc.fill))
			if err != nil {
				t.Fatalf("意外错误: %v", err)
			}
			if state != tc.wantState {
				t.Fatalf("state = %q, want %q", state, tc.wantState)
			}
			if sCtx.TaskModel.Goal != tc.wantGoal {
				t.Fatalf("Goal = %q, want %q", sCtx.TaskModel.Goal, tc.wantGoal)
			}
		})
	}
}

func TestApplyPerceiveResult_EmptyIsFailure(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})
	state, err := sm.applyPerceiveResult(&StateContext{}, []byte("  "))
	if err == nil || state != "S_PERCEIVE_FAILED" {
		t.Fatalf("空输出必须走失败路径（A-01），got state=%q err=%v", state, err)
	}
}

func TestApplyReflectResult_StoresReflection(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})
	sCtx := &StateContext{}
	state, err := sm.applyReflectResult(sCtx, protocol.StateContext{},
		[]byte("```json\n{\"GoalAchieved\":false,\"Errors\":[\"文件不存在\"]}\n```"))
	if err != nil || state != "S_REFLECT_DONE" {
		t.Fatalf("state=%q err=%v", state, err)
	}
	if sCtx.Reflection == nil || sCtx.Reflection.GoalAchieved || len(sCtx.Reflection.Errors) != 1 {
		t.Fatalf("反思结论未写入 sCtx，Respond 将无法如实报告失败: %+v", sCtx.Reflection)
	}
}

func TestOnReflectFailure_ContinuesToRespond(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})
	state, err := sm.onReflectFailure(protocol.StateContext{}, errors.New("provider down"))
	if err != nil || state != "S_REFLECT_DONE" {
		t.Fatalf("反思尽力而为：失败应继续进入 S_RESPOND，got state=%q err=%v", state, err)
	}
}

func TestOnRespondSuccess(t *testing.T) {
	if state, err := onRespondSuccess(protocol.StateContext{}, []byte("你好！")); err != nil || state != "S_RESPOND_DONE" {
		t.Fatalf("state=%q err=%v", state, err)
	}
	if state, err := onRespondSuccess(protocol.StateContext{}, []byte(" \n")); err == nil || state != "S_RESPOND_FAILED" {
		t.Fatalf("空回复必须失败（A-01），got state=%q err=%v", state, err)
	}
}

func TestParsePlanOnSuccess_EmptyPlanRoutesToRespond(t *testing.T) {
	sCtx := &StateContext{}
	state, err := parsePlanOnSuccess(sCtx, protocol.StateContext{}, []byte(`{"nodes":[],"edges":[]}`))
	if err != nil || state != "S_PLAN_EMPTY" {
		t.Fatalf("空 DAG 应转直答，got state=%q err=%v", state, err)
	}
}

func TestRespondEffect_IsTheOnlyUserAudience(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})
	sCtx := &StateContext{}
	if eff := sm.respondEffect(sCtx); eff.Audience != protocol.AudienceUser {
		t.Fatalf("S_RESPOND 必须是 AudienceUser")
	}
	// par_inv_06：其余带 LLMFillEffect 的转移一律 Internal。
	for from, m := range sm.transitions {
		for trig, tr := range m {
			if tr.Effects == nil {
				continue
			}
			effs, _ := tr.Effects(t.Context(), &StateContext{TaskModel: &TaskModel{}})
			for _, e := range effs {
				llm, ok := e.(protocol.LLMFillEffect)
				if !ok {
					continue
				}
				isRespond := tr.To == types.AgentStateRespond
				if isRespond != (llm.Audience == protocol.AudienceUser) {
					t.Errorf("转移 %d --%d--> %d：audience=%d，只有进入 S_RESPOND 的 Effect 可面向用户",
						from, trig, tr.To, llm.Audience)
				}
			}
		}
	}
}

func TestCompleteOnlyReachableFromRespond(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})
	for from, m := range sm.transitions {
		for _, tr := range m {
			if tr.To == types.AgentStateComplete && from != types.AgentStateRespond {
				t.Errorf("par_inv_07：S_COMPLETE 只能由 S_RESPOND 进入，发现来自 %d", from)
			}
		}
	}
}

func TestRespondEffect_RetriesEmptyReplyOnce(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})
	sCtx := &StateContext{}
	eff := sm.respondEffect(sCtx)

	if state, err := eff.OnSuccess(protocol.StateContext{}, []byte("  ")); err != nil || state != "S_RESPOND_RETRY" {
		t.Fatalf("首次空回复应自环重试，got state=%q err=%v", state, err)
	}
	if state, err := eff.OnSuccess(protocol.StateContext{}, []byte("")); err == nil || state != "S_RESPOND_FAILED" {
		t.Fatalf("超过 MaxRetry 后必须失败（A-01），got state=%q err=%v", state, err)
	}
	// 自环转移存在且重新挂 respond Effect
	if _, err := sm.Dispatch(t.Context(), sCtx, types.TriggerIntentReceived); err != nil {
		t.Fatal(err)
	}
	if tr, ok := sm.transitions[types.AgentStateRespond][types.TriggerRespondReady]; !ok || tr.To != types.AgentStateRespond {
		t.Fatal("缺少 S_RESPOND 自环重试转移")
	}
}

// TestReplanTransition_SingleReplanDone 复现 2026-09-25 实测：进入 S_REPLAN 时
// 转移声明的占位 Effect 与 handleReplanTransition 追加的 Effect 各产出一次
// S_REPLAN_DONE，第二次在 S_PLAN 命中 no transition，Run() 带错退出、流挂死。
func TestReplanTransition_SingleReplanDone(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})
	sCtx := &StateContext{MaxReplan: 3, TaskModel: &TaskModel{}}
	for _, trig := range []types.AgentTrigger{types.TriggerIntentReceived, types.TriggerPerceiveDone, types.TriggerPlanDone} {
		if _, err := sm.Dispatch(t.Context(), sCtx, trig); err != nil {
			t.Fatalf("dispatch %d: %v", trig, err)
		}
	}
	effects, err := sm.Dispatch(t.Context(), sCtx, types.TriggerValidateFail)
	if err != nil {
		t.Fatalf("S_VALIDATE → S_REPLAN: %v", err)
	}
	replanDone := 0
	for _, e := range effects {
		if det, ok := e.(protocol.DeterministicEffect); ok {
			if st, _ := det.Fn(t.Context(), protocol.StateContext{}); st == "S_REPLAN_DONE" {
				replanDone++
			}
		}
	}
	if replanDone != 1 {
		t.Fatalf("进入 S_REPLAN 应恰好产出 1 次 S_REPLAN_DONE，实际 %d", replanDone)
	}
}
