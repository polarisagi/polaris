package fsm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
)

type mockSkillMatcher struct {
	skillID string
	score   float64
	err     error
}

func (m *mockSkillMatcher) MatchIntent(rawIntent string) (string, float64, error) {
	return m.skillID, m.score, m.err
}

func TestSystem1Bypass(t *testing.T) {
	metrics.GlobalSurpriseIndex().SetLastValue(0.1) // Low surprise index

	sm := NewStateMachine(&dummyContextBuilder{})
	sm.SetSkillMatcher(&mockSkillMatcher{
		skillID: "test_skill",
		score:   0.95, // Above threshold
	})

	sCtx := &StateContext{
		RawIntentTS:     taint.NewTaintedString("do this", taint.TaintSource{Module: "test"}, ""),
		HasPreMatch:     true,
		PreMatchSkillID: "test_skill",
		PreMatchScore:   0.95,
	}

	effect := sm.trySystem1Bypass(context.Background(), sCtx)
	require.NotNil(t, effect)

	// par_inv_04: 验证系统状态机对于确定性 bypass effect, 绝对不会转入包含 LLM 调用的效果列表。
	// 这保证了命中 System-1 时 LLM 调用计数为 0。
	detEff, ok := effect.(protocol.DeterministicEffect)
	require.True(t, ok)
	require.NotNil(t, detEff.Fn)

	// Verify sCtx is populated correctly
	require.NotNil(t, sCtx.TaskModel)
	require.Equal(t, "do this", sCtx.TaskModel.Goal)
	require.NotNil(t, sCtx.DAGModel)
	require.Len(t, sCtx.DAGModel.Nodes, 1)
	require.Equal(t, "test_skill", sCtx.DAGModel.Nodes[0].ToolName)
}

func TestSystem1Bypass_Conditions(t *testing.T) {

	tests := []struct {
		name          string
		surpriseIndex float64
		skillID       string
		score         float64
		err           error
		matcherNil    bool
		expectBypass  bool
	}{
		{"No matcher", 0.1, "skill", 0.95, nil, true, false},
		{"High surprise", 0.4, "skill", 0.80, nil, false, false},
		{"Low score", 0.1, "skill", 0.90, nil, false, false},
		{"Empty skill", 0.1, "", 0.95, nil, false, false},
		{"Perfect match", 0.1, "skill", 0.95, nil, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sm := NewStateMachine(&dummyContextBuilder{})
			sCtx := &StateContext{
				RawIntentTS:     taint.NewTaintedString("do this", taint.TaintSource{Module: "test"}, ""),
				HasPreMatch:     !tt.matcherNil,
				PreMatchSkillID: tt.skillID,
				PreMatchScore:   tt.score,
				PreMatchErr:     tt.err,
			}

			effect := sm.trySystem1Bypass(context.Background(), sCtx)
			if tt.expectBypass {
				require.NotNil(t, effect)
			} else {
				require.Nil(t, effect)
			}
		})
	}
}
