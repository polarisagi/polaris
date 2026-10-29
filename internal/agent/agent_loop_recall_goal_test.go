package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// stubKnowledge 固定返回一条 RAG 命中，并记录被查询的 query。
type stubKnowledge struct {
	queries []string
}

func (s *stubKnowledge) SearchRAG(_ context.Context, q string, _ int) ([]fsm.KnowledgeResult, error) {
	s.queries = append(s.queries, q)
	return []fsm.KnowledgeResult{{Content: "知识库条目：备份必须启用 WAL 归档", Source: "kb://ops", Score: 0.9}}, nil
}

// TestAgenticLoopRecall_GoalEnablesRAGAndRaisesTaint 验证 R5-03：
// agentic 召回传入 Goal 后会查 RAG 知识库，命中内容出现在第 1 轮首条 user 消息里；
// 召回非空后 GlobalTaintLevel 至少为 TaintMedium（与 staged 的 writePlanRecall 同口径）。
func TestAgenticLoopRecall_GoalEnablesRAGAndRaisesTaint(t *testing.T) {
	// RAG 默认要求 SurpriseIndex >= recall.rag_min_surprise；测试里置 0 使其确定性触发。
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	prev := config.Get()
	cfg := config.Config{}
	cfg.Thresholds = config.DefaultThresholds()
	cfg.Thresholds.M4Kernel.RecallRAGMinSurprise = 0
	cfg.Thresholds.M4Kernel.LoopMode = "agentic"
	config.Update(&cfg)
	t.Cleanup(func() { config.Update(prev) })

	mem := &testRecallMem{
		mockMemoryForIntegration: &mockMemoryForIntegration{
			episodic: &mockEpisodicMemForIntegration{},
			working:  &mockWorkingMemForIntegration{immutable: &mockImmutableCoreForIntegration{}},
		},
	}
	prov := &recallTestProvider{replies: []*types.ProviderResponse{{Content: "已按知识库给出方案。"}}}
	a := setupRecallAgent(t, "test-recall-goal", mem, nil, prov)
	ks := &stubKnowledge{}
	a.SetKnowledgeSearcher(ks)
	a.SetTaskIntent(taint.NewTaintedString("如何做数据库备份？", taint.TaintSource{OriginTaintLevel: types.TaintLow}, "test"))

	runAgenticTurnWait(t, a)

	require.NotEmpty(t, ks.queries, "Goal 非空时应查询 RAG 知识库")
	reqs := prov.requests()
	require.NotEmpty(t, reqs)
	var firstUser *types.Message
	for i := range reqs[0] {
		if reqs[0][i].Role == "user" {
			firstUser = &reqs[0][i]
			break
		}
	}
	require.NotNil(t, firstUser)
	require.True(t, strings.Contains(firstUser.Content, "WAL 归档"), "首条 user 消息应含 RAG 命中: %q", firstUser.Content)

	a.sCtx.Mu.RLock()
	global := a.sCtx.GlobalTaintLevel
	a.sCtx.Mu.RUnlock()
	require.GreaterOrEqual(t, global, types.TaintMedium, "召回非空后全局污点应至少为 TaintMedium")
}
