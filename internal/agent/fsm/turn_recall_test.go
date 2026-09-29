package fsm

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// 回合起点必须清空上一回合的召回：召回只在回合内复用（ADR-0105 决策四），
// 带入下一回合会让新问题看到与之无关的旧召回，并跳过本应发起的检索。
func TestTurnStartClearsTurnRecall(t *testing.T) {
	sm := NewStateMachine(&dummyContextBuilder{})
	sCtx := &StateContext{
		TurnRecall:  &TurnRecall{EpisodicDone: true, GoalDone: true, Text: "上一回合的召回"},
		RawIntentTS: taint.NewTaintedString("帮我列出目录", taint.TaintSource{OriginTaintLevel: types.TaintHigh}, "t"),
	}
	tr, ok := sm.transitions[types.AgentStateIdle][types.TriggerIntentReceived]
	require.True(t, ok)
	_, err := tr.Effects(context.Background(), sCtx)
	require.NoError(t, err)
	require.Nil(t, sCtx.TurnRecall)
}

func TestTurnRecallClone_NilSafeAndIndependent(t *testing.T) {
	var nilRecall *TurnRecall
	c := nilRecall.Clone()
	require.NotNil(t, c)
	require.False(t, c.EpisodicDone)

	orig := &TurnRecall{}
	orig.Items[RecallEpisodic] = []RecallItem{{Text: "- a", Key: "a"}}
	cp := orig.Clone()
	cp.Items[RecallEpisodic][0].Text = "- changed"
	cp.Items[RecallEpisodic] = append(cp.Items[RecallEpisodic], RecallItem{Text: "- b", Key: "b"})
	require.Equal(t, "- a", orig.Items[RecallEpisodic][0].Text)
	require.Len(t, orig.Items[RecallEpisodic], 1)
}

func TestRecallKinds_PriorityOrder(t *testing.T) {
	require.Equal(t,
		[]RecallKind{RecallReflection, RecallEpisodic, RecallSemantic, RecallRAG, RecallProfile},
		RecallKinds())
}

func TestExecuteResultForPrompt(t *testing.T) {
	t.Run("未超限原样返回", func(t *testing.T) {
		sCtx := &StateContext{ExecuteResult: []byte(`{"ok":true}`)}
		require.Equal(t, `{"ok":true}`, string(ExecuteResultForPrompt(sCtx)))
	})

	t.Run("超限时取带取回提示的最近观察", func(t *testing.T) {
		obs := "[output truncated: 7000 bytes total; full output: read_tool_ref(task_id=\"t\", id=\"x\")]\nhead…tail"
		sCtx := &StateContext{
			ExecuteResult: []byte(strings.Repeat("A", 7000)),
			Observations:  []string{"older", obs},
		}
		got := string(ExecuteResultForPrompt(sCtx))
		require.Equal(t, obs, got)
		require.LessOrEqual(t, len(got), ObservationMaxBytes)
	})

	t.Run("超限且带污点告警时告警原样保留", func(t *testing.T) {
		sCtx := &StateContext{
			ExecuteResult: append([]byte(strings.Repeat("B", 6000)), HighTaintWarning...),
			Observations:  []string{"short observation"},
		}
		got := string(ExecuteResultForPrompt(sCtx))
		require.Equal(t, "short observation"+HighTaintWarning, got)
	})

	t.Run("无观察时头尾保留截断且不超上限", func(t *testing.T) {
		sCtx := &StateContext{
			ExecuteResult: append([]byte("HEAD"+strings.Repeat("C", 7000)+"TAIL"), HighTaintWarning...),
		}
		got := string(ExecuteResultForPrompt(sCtx))
		require.True(t, strings.HasSuffix(got, HighTaintWarning))
		body := strings.TrimSuffix(got, HighTaintWarning)
		require.LessOrEqual(t, len(body), ObservationMaxBytes)
		require.True(t, strings.HasPrefix(body, "HEAD"))
		require.True(t, strings.HasSuffix(body, "TAIL"))
	})
}
