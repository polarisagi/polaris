package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// recallTestProvider 记录发往 LLM 的每一轮请求，并按预设响应出队。
type recallTestProvider struct {
	mu      sync.Mutex
	reqs    [][]types.Message
	replies []*types.ProviderResponse
	callIdx int
}

func (p *recallTestProvider) Infer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (*types.ProviderResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	copied := make([]types.Message, len(msgs))
	copy(copied, msgs)
	p.reqs = append(p.reqs, copied)

	if p.callIdx < len(p.replies) {
		resp := p.replies[p.callIdx]
		p.callIdx++
		return resp, nil
	}
	return &types.ProviderResponse{Content: "default completion"}, nil
}

func (p *recallTestProvider) StreamInfer(ctx context.Context, msgs []types.Message, opts ...types.InferOption) (<-chan types.StreamEvent, error) {
	// custom-nolint:bare-infer
	resp, err := p.Infer(ctx, msgs, opts...)
	if err != nil {
		return nil, err
	}
	ch := make(chan types.StreamEvent, 10)
	if resp.Content != "" {
		ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: resp.Content}
	}
	for _, tc := range resp.ToolCalls {
		rawInput := tc.Input
		if len(rawInput) == 0 {
			rawInput = []byte("{}")
		}
		payload, _ := json.Marshal(map[string]any{
			"id":    tc.ID,
			"name":  tc.Name,
			"input": json.RawMessage(rawInput),
		})
		ch <- types.StreamEvent{Type: types.StreamToolCall, Content: string(payload)}
	}
	close(ch)
	return ch, nil
}

func (p *recallTestProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{SupportsStreaming: true, SupportsTools: true}
}
func (p *recallTestProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (p *recallTestProvider) ModelID() string                      { return "recall-test-provider" }

func (p *recallTestProvider) requests() [][]types.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	res := make([][]types.Message, len(p.reqs))
	copy(res, p.reqs)
	return res
}

// testRecallCog 模拟认知语义检索，返回指定的 episodic 命中 ID。
type testRecallCog struct {
	docID string
	err   error
}

func (c *testRecallCog) FTSSearch(context.Context, string, int) ([]fsm.CogResult, error) {
	return nil, nil
}

func (c *testRecallCog) FTSEpisodic(context.Context, string, int) ([]fsm.CogResult, error) {
	if c.err != nil {
		return nil, c.err
	}
	if c.docID == "" {
		return nil, nil
	}
	return []fsm.CogResult{{DocID: c.docID, Score: 7.5}}, nil
}

// testRecallMem 模拟 MemoryFacade，支持返回情景事件或报错。
type testRecallMem struct {
	*mockMemoryForIntegration
	docID      string
	event      types.Event
	listErr    error
	listCalled bool
}

func (m *testRecallMem) EpisodicProjectOf(_ context.Context, id string) (string, bool) {
	return types.DefaultProjectID, id == m.docID
}

func (m *testRecallMem) ListEpisodicEvents(_ context.Context, q types.EpisodicQuery) ([]types.ScoredEvent, error) {
	m.listCalled = true
	if m.listErr != nil {
		return nil, m.listErr
	}
	if len(q.IDs) == 0 {
		return nil, nil
	}
	for _, id := range q.IDs {
		if id == m.docID {
			return []types.ScoredEvent{{Score: 1.0, Event: &m.event}}, nil
		}
	}
	return nil, nil
}

func setupRecallAgent(t *testing.T, agentID string, mem *testRecallMem, cog *testRecallCog, prov *recallTestProvider) *Agent {
	t.Helper()
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	a := NewAgentWithDefaults(agentID)
	a.InjectProvider(prov)
	a.InjectPolicyGate(&allowPolicyGate{})
	a.InjectToolExecutor(capToolExecutor{
		"read_file":  types.CapReadOnly,
		"write_file": types.CapWriteLocal,
	})
	if mem != nil {
		a.InjectMemory(mem)
	}
	if cog != nil {
		a.SetCognitiveSearcher(cog)
	}
	return a
}

// TestAgenticLoopRecall_FirstRoundContainsRecallOnlyOnce 验证 R4-02：
// 第 1 轮请求的首条 user 消息含召回内容，且 "## Recalled Context" 仅出现 1 次。
func TestAgenticLoopRecall_FirstRoundContainsRecallOnlyOnce(t *testing.T) {
	mem := &testRecallMem{
		mockMemoryForIntegration: &mockMemoryForIntegration{
			episodic: &mockEpisodicMemForIntegration{},
			working:  &mockWorkingMemForIntegration{immutable: &mockImmutableCoreForIntegration{}},
		},
		docID: "ev_backup_01",
		event: types.Event{
			ID:        "ev_backup_01",
			Type:      "task_done",
			Payload:   []byte(`{"summary":"上次数据库备份使用了 pg_dump 归档"}`),
			CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	cog := &testRecallCog{docID: "ev_backup_01"}
	prov := &recallTestProvider{
		replies: []*types.ProviderResponse{
			{Content: "我已基于之前的备份经验为您准备好方案。"},
		},
	}

	a := setupRecallAgent(t, "test-recall-once", mem, cog, prov)
	a.SetTaskIntent(taint.NewTaintedString("如何进行数据库备份？", taint.TaintSource{OriginTaintLevel: types.TaintLow}, "test"))

	runAgenticTurnWait(t, a)

	reqs := prov.requests()
	require.NotEmpty(t, reqs, "应至少记录 1 轮 LLM 请求")

	r1Msgs := reqs[0]
	var firstUserMsg *types.Message
	for i := range r1Msgs {
		if r1Msgs[i].Role == "user" {
			firstUserMsg = &r1Msgs[i]
			break
		}
	}
	require.NotNil(t, firstUserMsg, "第 1 轮请求应包含 user 消息")
	require.Contains(t, firstUserMsg.Content, "## Recalled Context", "首条 user 消息应包含召回段标题")
	require.Contains(t, firstUserMsg.Content, "pg_dump 归档", "首条 user 消息应包含召回的情景内容")
	require.Contains(t, firstUserMsg.Content, "如何进行数据库备份？", "首条 user 消息应包含用户原意图")
	require.Equal(t, 1, strings.Count(firstUserMsg.Content, "## Recalled Context"), "## Recalled Context 只能出现恰好 1 次")
}

// TestAgenticLoopRecall_MonotonicPrefix 验证 R4-02：
// 多轮循环中第 2、3 轮请求的前缀单调稳定（T1）。
func TestAgenticLoopRecall_MonotonicPrefix(t *testing.T) {
	mem := &testRecallMem{
		mockMemoryForIntegration: &mockMemoryForIntegration{
			episodic: &mockEpisodicMemForIntegration{},
			working:  &mockWorkingMemForIntegration{immutable: &mockImmutableCoreForIntegration{}},
		},
		docID: "ev_mono_01",
		event: types.Event{
			ID:        "ev_mono_01",
			Type:      "task_done",
			Payload:   []byte(`{"summary":"生产环境必须开启 WAL 归档"}`),
			CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	cog := &testRecallCog{docID: "ev_mono_01"}
	prov := &recallTestProvider{
		replies: []*types.ProviderResponse{
			// 第 1 轮：模型调用 read_file
			{
				ToolCalls: []types.InferToolCall{
					{ID: "c1", Name: "read_file", Input: []byte(`{"path":"config.json"}`)},
				},
			},
			// 第 2 轮：模型再次调用 read_file
			{
				ToolCalls: []types.InferToolCall{
					{ID: "c2", Name: "read_file", Input: []byte(`{"path":"schema.sql"}`)},
				},
			},
			// 第 3 轮：给出最终文本回复
			{
				Content: "配置与表结构已检查完毕，备份已就绪。",
			},
		},
	}

	a := setupRecallAgent(t, "test-recall-mono", mem, cog, prov)
	a.SetTaskIntent(taint.NewTaintedString("检查备份配置", taint.TaintSource{OriginTaintLevel: types.TaintLow}, "test"))

	runAgenticTurnWait(t, a)

	reqs := prov.requests()
	require.Equal(t, 3, len(reqs), "期望恰好 3 轮请求")

	r1, r2, r3 := reqs[0], reqs[1], reqs[2]

	// T1 前缀单调断言：r1 的全部消息内容逐条作为 r2 的严格前缀
	require.True(t, len(r2) > len(r1), "r2 消息数应大于 r1")
	for i := range r1 {
		require.Equal(t, r1[i].Role, r2[i].Role, "第 %d 条消息 Role 必须在前缀中保持一致", i)
		require.Equal(t, r1[i].Content, r2[i].Content, "第 %d 条消息 Content 必须在前缀中完全一致", i)
	}

	// r2 的全部消息内容逐条作为 r3 的严格前缀
	require.True(t, len(r3) > len(r2), "r3 消息数应大于 r2")
	for i := range r2 {
		require.Equal(t, r2[i].Role, r3[i].Role, "第 %d 条消息 Role 必须在前缀中保持一致", i)
		require.Equal(t, r2[i].Content, r3[i].Content, "第 %d 条消息 Content 必须在前缀中完全一致", i)
	}

	// 召回内容在整场多轮对话中只在首条 user 消息出现 1 次
	totalRecallCount := 0
	for _, m := range r3 {
		totalRecallCount += strings.Count(m.Content, "## Recalled Context")
	}
	require.Equal(t, 1, totalRecallCount, "多轮请求中 ## Recalled Context 整体只应出现 1 次")
}

// TestAgenticLoopRecall_HighTaintBlocksWriteTool 验证 R4-02：
// 召回内容为 TaintHigh 时，提升会话全局污点，并在后续轮次按 TaintHigh 校验写类工具。
func TestAgenticLoopRecall_HighTaintBlocksWriteTool(t *testing.T) {
	mem := &testRecallMem{
		mockMemoryForIntegration: &mockMemoryForIntegration{
			episodic: &mockEpisodicMemForIntegration{},
			working:  &mockWorkingMemForIntegration{immutable: &mockImmutableCoreForIntegration{}},
		},
		docID: "ev_taint_high",
		event: types.Event{
			ID:         "ev_taint_high",
			Type:       "task_done",
			Payload:    []byte(`{"summary":"包含不受信任输入的高污点历史事件"}`),
			TaintLevel: types.TaintHigh,
			CreatedAt:  time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		},
	}
	cog := &testRecallCog{docID: "ev_taint_high"}
	prov := &recallTestProvider{
		replies: []*types.ProviderResponse{
			// 第 1 轮：模型调用只读工具 read_file（TaintHigh 下允许）
			{
				ToolCalls: []types.InferToolCall{
					{ID: "c_read", Name: "read_file", Input: []byte(`{"path":"safe.txt"}`)},
				},
			},
			// 第 2 轮：模型试图调用写类工具 write_file（TaintHigh 下被 L1_taint 拦截）
			{
				ToolCalls: []types.InferToolCall{
					{ID: "c_write", Name: "write_file", Input: []byte(`{"path":"danger.sh","content":"rm -rf /"}`)},
				},
			},
			// 第 3 轮：被拦截报错后模型给出收尾回复
			{
				Content: "写入被安全策略拦截，停止操作。",
			},
		},
	}

	a := setupRecallAgent(t, "test-recall-taint", mem, cog, prov)
	a.SetTaskIntent(taint.NewTaintedString("同步脚本", taint.TaintSource{OriginTaintLevel: types.TaintHigh}, "test"))

	runAgenticTurnWait(t, a)

	// 断言全局污点已被提升为 TaintHigh
	a.sCtx.Mu.Lock()
	globalTaint := a.sCtx.GlobalTaintLevel
	msgs := make([]types.Message, len(a.sCtx.LoopMessages))
	copy(msgs, a.sCtx.LoopMessages)
	a.sCtx.Mu.Unlock()

	require.Equal(t, types.TaintHigh, globalTaint, "召回 TaintHigh 内容后全局污点应升为 TaintHigh")

	// 断言第 2 轮 write_file 调用由于 TaintHigh 校验未通过而被 L1_taint 拦截
	var writeResultMsg *types.Message
	for i := range msgs {
		if msgs[i].Role == "user" && strings.Contains(msgs[i].Content, "Forbidden: action rejected by security policy") {
			writeResultMsg = &msgs[i]
			break
		}
	}
	require.NotNil(t, writeResultMsg, "期望在消息中记录写类工具被校验链拦截的错误响应")
	require.Contains(t, writeResultMsg.Content, "L1_taint", "拦截原因应为 L1_taint 高污点写工具限制")
}

// TestAgenticLoopRecall_FailureDegradesGracefully 验证 R4-02：
// 召回报错时降级为不召回，回合正常完成且打指标。
func TestAgenticLoopRecall_FailureDegradesGracefully(t *testing.T) {
	mem := &testRecallMem{
		mockMemoryForIntegration: &mockMemoryForIntegration{
			episodic: &mockEpisodicMemForIntegration{},
			working:  &mockWorkingMemForIntegration{immutable: &mockImmutableCoreForIntegration{}},
		},
		docID:   "ev_fail",
		listErr: errors.New("database connection refused"),
	}
	cog := &testRecallCog{docID: "ev_fail"}
	prov := &recallTestProvider{
		replies: []*types.ProviderResponse{
			{Content: "虽然召回服务故障，我依然可以直接为您解答。"},
		},
	}

	a := setupRecallAgent(t, "test-recall-degrade", mem, cog, prov)
	a.SetTaskIntent(taint.NewTaintedString("请问今天是几号？", taint.TaintSource{OriginTaintLevel: types.TaintLow}, "test"))

	runAgenticTurnWait(t, a)

	// 确认回合正常完成
	require.Equal(t, types.AgentStateComplete, a.sm.Current(), "召回失败时回合仍应正常进入 Complete")

	reqs := prov.requests()
	require.NotEmpty(t, reqs, "应有请求发出")

	r1UserMsg := reqs[0][len(reqs[0])-1]
	require.NotContains(t, r1UserMsg.Content, "## Recalled Context", "召回失败降级后不应包含 Recalled Context")
	require.Contains(t, r1UserMsg.Content, "请问今天是几号？", "降级后应直接使用用户原文")

	a.sCtx.Mu.Lock()
	turnRecall := a.sCtx.TurnRecall
	a.sCtx.Mu.Unlock()
	require.True(t, turnRecall == nil || turnRecall.Text == "", "召回失败降级时 TurnRecall 应为空或无文本")
}
