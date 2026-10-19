package agent

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// memCheckpointRepo 线程安全的 TaskCheckpointRepository 内存实现，用于崩溃续跑测试。
type memCheckpointRepo struct {
	mu   sync.Mutex
	rows []types.TaskCheckpointRow
}

func (m *memCheckpointRepo) GetCheckpoint(_ context.Context, taskID, nodeID string, attempt int) (*types.TaskCheckpointRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.TaskID == taskID && r.NodeID == nodeID && r.Attempt == attempt {
			cp := r
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *memCheckpointRepo) GetLatestCheckpoint(_ context.Context, taskID, nodeID string) (*types.TaskCheckpointRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var latest *types.TaskCheckpointRow
	for i := range m.rows {
		r := &m.rows[i]
		if r.TaskID == taskID && r.NodeID == nodeID {
			if latest == nil || r.StartedAt > latest.StartedAt {
				latest = r
			}
		}
	}
	if latest != nil {
		cp := *latest
		return &cp, nil
	}
	return nil, nil
}

func (m *memCheckpointRepo) UpsertCheckpoint(_ context.Context, row types.TaskCheckpointRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.rows {
		if m.rows[i].TaskID == row.TaskID && m.rows[i].NodeID == row.NodeID && m.rows[i].Attempt == row.Attempt {
			m.rows[i] = row
			return nil
		}
	}
	m.rows = append(m.rows, row)
	return nil
}

func (m *memCheckpointRepo) ListCheckpointsByTask(_ context.Context, taskID string) ([]types.TaskCheckpointRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []types.TaskCheckpointRow
	for _, r := range m.rows {
		if r.TaskID == taskID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *memCheckpointRepo) ListByStatus(_ context.Context, status string) ([]types.TaskCheckpointRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []types.TaskCheckpointRow
	for _, r := range m.rows {
		if r.Status == status {
			out = append(out, r)
		}
	}
	return out, nil
}

// recordingToolExecutor 记录执行过的工具名称及输入。
type recordingToolExecutor struct {
	mu       sync.Mutex
	executed []string
}

func (r *recordingToolExecutor) Lookup(name string) (types.Tool, error) {
	return types.Tool{Name: name, Source: types.ToolBuiltin, Capability: types.CapReadOnly}, nil
}

func (r *recordingToolExecutor) ExecuteWithTaint(_ context.Context, name string, input []byte, _ types.TaintLevel) (*types.ToolResult, error) {
	r.mu.Lock()
	r.executed = append(r.executed, name)
	r.mu.Unlock()
	return &types.ToolResult{Success: true, Output: []byte(fmt.Sprintf(`{"executed":%q}`, name))}, nil
}

func (r *recordingToolExecutor) getExecuted() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.executed))
	copy(out, r.executed)
	return out
}

// TestP1_AgenticLoop_CrashRecovery_NoDuplicateToolExecution 验证 P1 / HE-6：
// 模拟第 3 轮工具执行后崩溃 → 重建 Agent → 从快照恢复 →
// 断言消息序列与崩溃前一致、第 1~3 轮的工具没有被再次调用、能继续跑完第 4 轮。
func TestP1_AgenticLoop_CrashRecovery_NoDuplicateToolExecution(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	sessionID := "test-session-crash-recovery"
	repo := &memCheckpointRepo{}

	// --- 阶段一：运行 Agent 1 执行第 1~3 轮工具调用后模拟崩溃 ---
	exec1 := &recordingToolExecutor{}
	prov1 := &loopTestProvider{
		responses: []*types.ProviderResponse{
			// 第 1 轮：调用 echo_1
			{ToolCalls: []types.InferToolCall{{ID: "c1", Name: "echo_1", Input: []byte(`{}`)}}},
			// 第 2 轮：调用 echo_2
			{ToolCalls: []types.InferToolCall{{ID: "c2", Name: "echo_2", Input: []byte(`{}`)}}},
			// 第 3 轮：调用 echo_3
			{ToolCalls: []types.InferToolCall{{ID: "c3", Name: "echo_3", Input: []byte(`{}`)}}},
		},
	}

	agent1 := NewAgentWithDefaults(sessionID)
	agent1.InjectTaskCheckpointRepo(repo)
	agent1.InjectProvider(prov1)
	agent1.InjectPolicyGate(&allowPolicyGate{})
	agent1.InjectToolExecutor(exec1)

	// 启动 Agent 1
	ctx, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	go func() {
		_ = agent1.Run(ctx)
	}()

	if err := agent1.SendIntent(types.TriggerIntentReceived); err != nil {
		t.Fatalf("agent1 SendIntent 失败: %v", err)
	}

	// 等待第 3 轮工具执行完成
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(exec1.getExecuted()) >= 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(exec1.getExecuted()) < 3 {
		t.Fatalf("Agent 1 在崩溃前未完成 3 轮工具执行，实际执行: %v", exec1.getExecuted())
	}

	// 模拟进程崩溃：直接取消 Agent 1 的上下文
	cancel1()

	// 验证快照仓库中是否有持久化的轮次快照（P1 核心证据）
	cps, err := repo.ListCheckpointsByTask(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("查询快照失败: %v", err)
	}
	var hasRound3Snapshot bool
	for _, cp := range cps {
		if cp.NodeID == "loop_round_3" && cp.ResumeCtxJSON != "" {
			hasRound3Snapshot = true
			break
		}
	}
	if !hasRound3Snapshot {
		t.Fatal("P1 缺陷：第 3 轮结束后未将增量快照持久化至 TaskCheckpointRepository！")
	}

	// --- 阶段二：进程重启，新建 Agent 2 从快照恢复续跑 ---
	exec2 := &recordingToolExecutor{}
	prov2 := &loopTestProvider{
		responses: []*types.ProviderResponse{
			// 第 4 轮：调用 echo_4
			{ToolCalls: []types.InferToolCall{{ID: "c4", Name: "echo_4", Input: []byte(`{}`)}}},
			// 第 5 轮：输出最终总结文本收尾
			{Content: "全部 4 个工具步骤已执行完毕，任务成功。"},
		},
	}

	agent2 := NewAgentWithDefaults(sessionID)
	agent2.InjectTaskCheckpointRepo(repo)
	agent2.InjectProvider(prov2)
	agent2.InjectPolicyGate(&allowPolicyGate{})
	agent2.InjectToolExecutor(exec2)

	// 从持久化快照恢复
	restored := agent2.ResumeAgenticLoopFromCheckpoint(context.Background())
	if !restored {
		t.Fatal("Agent 2 从快照恢复失败")
	}

	// 验证消息序列与崩溃前一致
	agent2.sCtx.Mu.Lock()
	r2Msgs := make([]types.Message, len(agent2.sCtx.LoopMessages))
	copy(r2Msgs, agent2.sCtx.LoopMessages)
	r2Round := agent2.sCtx.LoopRound
	agent2.sCtx.Mu.Unlock()

	if r2Round != 3 {
		t.Fatalf("恢复后轮次期望为 3，实际为: %d", r2Round)
	}
	if len(r2Msgs) < 6 { // 至少 3 轮的 assistant + tool 消息
		t.Fatalf("恢复后消息条数不足，实际: %d", len(r2Msgs))
	}

	// 启动 Agent 2 继续执行
	done2 := make(chan struct{})
	go func() {
		_ = agent2.Run(context.Background())
		close(done2)
	}()

	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("Agent 2 续跑超时")
	}

	// 断言：第 1~3 轮工具没有被再次调用，Agent 2 仅执行了 echo_4！
	exec2Tools := exec2.getExecuted()
	if len(exec2Tools) != 1 || exec2Tools[0] != "echo_4" {
		t.Fatalf("工具重复执行或未执行第 4 轮工具，Agent 2 实际执行: %v，期望: [echo_4]", exec2Tools)
	}

	if st := agent2.sm.Current(); st != types.AgentStateComplete {
		t.Fatalf("Agent 2 续跑后期望达到 Complete 状态，实际: %v", st)
	}
}
