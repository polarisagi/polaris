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
