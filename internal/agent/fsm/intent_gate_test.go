package fsm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestClassifyIntentWeight(t *testing.T) {
	cases := []struct {
		in   string
		want IntentWeight
	}{
		{"你好", IntentPhatic},
		{"  你好呀~ ", IntentPhatic},
		{"Hello!", IntentPhatic},
		{"谢谢啦！", IntentPhatic},
		{"Thank you.", IntentPhatic},
		{"晚安😴", IntentPhatic},
		{"好的", IntentAck},
		{"OK", IntentAck},
		{"同意。", IntentAck},
		{"好吧", IntentAck},
		{"go ahead", IntentAck},
		// 必须走完整路径：带任务语义、超长、空串、仅标点
		{"你好，帮我看下这个报错", IntentFull},
		{"谢谢，再把日志删掉", IntentFull},
		{"删除所有文件", IntentFull},
		{"", IntentFull},
		{"？？？", IntentFull},
		{"hello hello hello hello", IntentFull},
	}
	for _, c := range cases {
		require.Equal(t, c.want, ClassifyIntentWeight(c.in), "input=%q", c.in)
	}
}

// 寒暄旁路必须是确定性 Effect（零 LLM）且路由到直答；短确认与常规输入不得命中。
func TestTryPhaticBypass(t *testing.T) {
	mk := func(s string) *StateContext {
		return &StateContext{RawIntentTS: taint.NewTaintedString(s,
			taint.TaintSource{Module: "test", OriginTaintLevel: types.TaintHigh}, "")}
	}

	sCtx := mk("你好")
	eff := tryPhaticBypass(sCtx)
	require.NotNil(t, eff)
	det, ok := eff.(protocol.DeterministicEffect)
	require.True(t, ok, "phatic bypass must not be an LLMFillEffect")
	next, err := det.Fn(context.Background(), protocol.StateContext{})
	require.NoError(t, err)
	require.Equal(t, types.State("S_PERCEIVE_DIRECT"), next)
	require.NotNil(t, sCtx.TaskModel)
	require.NotNil(t, sCtx.TaskModel.NeedsTools)
	require.False(t, *sCtx.TaskModel.NeedsTools)

	require.Nil(t, tryPhaticBypass(mk("好的")), "ack may authorize a proposed action; must go through Perceive")
	require.Nil(t, tryPhaticBypass(mk("帮我查一下天气")))
	require.Nil(t, tryPhaticBypass(&StateContext{}))
}

// ADR-0101 决策四：仅"首轮 + 全部成功 + 0<Complexity<阈值"跳过 Reflect LLM；
// 任一条件不满足必须保留反思，跳过判据不得放过失败或复杂任务。
func TestTrySkipReflect(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})
	mk := func(ok bool, c float64) *StateContext {
		return &StateContext{ExecAllSucceeded: ok, TaskModel: &TaskModel{Goal: "g", Complexity: c},
			Reflection: &ReflectionModel{GoalAchieved: false}}
	}

	sCtx := mk(true, 0.2)
	eff := sm.trySkipReflect(sCtx)
	require.NotNil(t, eff)
	det, ok := eff.(protocol.DeterministicEffect)
	require.True(t, ok)
	next, err := det.Fn(context.Background(), protocol.StateContext{})
	require.NoError(t, err)
	require.Equal(t, types.State("S_REFLECT_DONE"), next)
	require.Nil(t, sCtx.Reflection, "上一轮反思结论不得混入本轮回复")

	require.Nil(t, sm.trySkipReflect(mk(false, 0.2)), "存在失败必须反思")
	require.Nil(t, sm.trySkipReflect(mk(true, 0.8)), "复杂任务必须反思")
	require.Nil(t, sm.trySkipReflect(mk(true, 0)), "复杂度缺失按保守路径反思")
	require.Nil(t, sm.trySkipReflect(&StateContext{ExecAllSucceeded: true}), "无 TaskModel 必须反思")

	sm.replanCount = 1
	require.Nil(t, sm.trySkipReflect(mk(true, 0.2)), "重规划轮次必须反思（观察—再规划）")
}

// ADR-0101 决策四 4b′：Perceive 直答合并只在 NeedsTools=false 且 Reply 可发布时生效；
// 只有 Perceive→Respond 入边消费 PreparedReply，其余入边照常调 Respond LLM。
func TestPerceiveDirectReplyMerge(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})

	sCtx := &StateContext{}
	st, err := sm.applyPerceiveResult(sCtx, []byte(`{"Goal":"问候","NeedsTools":false,"Reply":"你好！"}`))
	require.NoError(t, err)
	require.Equal(t, types.State("S_PERCEIVE_DIRECT"), st)
	require.Equal(t, "你好！", sCtx.PreparedReply)

	effs, err := sm.directRespondEffects(context.Background(), sCtx)
	require.NoError(t, err)
	require.Len(t, effs, 1)
	_, isDet := effs[0].(protocol.DeterministicEffect)
	require.True(t, isDet, "已有回复时不得再调 Respond LLM")

	// NeedsTools=true：Reply 一律忽略
	sCtx = &StateContext{}
	_, _ = sm.applyPerceiveResult(sCtx, []byte(`{"Goal":"读文件","NeedsTools":true,"Reply":"好的"}`))
	require.Empty(t, sCtx.PreparedReply)

	// 内部产物特征：退回 Respond LLM
	sCtx = &StateContext{}
	_, _ = sm.applyPerceiveResult(sCtx, []byte(`{"Goal":"x","NeedsTools":false,"Reply":"{\"Goal\":\"x\"}"}`))
	require.Empty(t, sCtx.PreparedReply)
	effs, _ = sm.directRespondEffects(context.Background(), sCtx)
	_, isLLM := effs[0].(protocol.LLMFillEffect)
	require.True(t, isLLM)
}

// 回合起点必须清空上一回合的直答：否则本回合走 System-1/FastPath/寒暄旁路到达
// Perceive→Respond 时会发布陈旧回复（4b′）。
func TestTurnStartClearsPreparedReply(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})
	sCtx := &StateContext{
		PreparedReply: "上一回合的回复",
		RawIntentTS:   taint.NewTaintedString("帮我列出目录", taint.TaintSource{OriginTaintLevel: types.TaintHigh}, "t"),
	}
	tr, ok := sm.transitions[types.AgentStateIdle][types.TriggerIntentReceived]
	require.True(t, ok)
	_, err := tr.Effects(context.Background(), sCtx)
	require.NoError(t, err)
	require.Empty(t, sCtx.PreparedReply)
}
