package agent

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/pkg/types"
)

func setupTestDBWithRoundsSchema(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	for _, f := range []string{"001_events.sql", "007_tasks.sql", "035_task_checkpoints.sql", "042_subagent_runs.sql", "051_agent_loop_rounds.sql"} {
		ddl, err := schema.FS.ReadFile(f)
		if err != nil {
			t.Fatalf("read schema %s: %v", f, err)
		}
		if _, err := db.Exec(string(ddl)); err != nil {
			t.Fatalf("apply schema %s: %v", f, err)
		}
	}
	return db
}

// TestR3_05_MultipleDelegatingCallsInSameBatch 验证生产装配下：
// 同一批中有 2 个 spawn（各派 2 个子智能体）和 1 个 read_file，
// 挂起等待全部 4 个子任务完成后恢复，恢复后 3 条 tool_result 与 3 个 tool_use_id 一一对应 (R3-05)。
func TestR3_05_MultipleDelegatingCallsInSameBatch(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")

	db := setupTestDBWithRoundsSchema(t)
	bb := orchestrator.NewSQLiteBlackboard(db)

	agent := NewAgentWithDefaults("test-r3-05-multi-delegation")
	agent.InjectHandoffPoster(bb)
	agent.InjectPolicyGate(&allowPolicyGate{})
	agent.extQuerier = db

	exec := newAwaitTestToolExecutor()
	agent.InjectToolExecutor(exec)

	prov := &loopTestProvider{
		responses: []*types.ProviderResponse{
			{
				Content: "派发两组并行子智能体并读取本地文件...",
				ToolCalls: []types.InferToolCall{
					{
						ID:   "call-spawn-1",
						Name: "spawn_agents",
						Input: []byte(`{
							"tasks": [
								{"role":"worker-a1","prompt":"do worker a1"},
								{"role":"worker-a2","prompt":"do worker a2"}
							],
							"wait": "all"
						}`),
					},
					{
						ID:   "call-spawn-2",
						Name: "spawn_agents",
						Input: []byte(`{
							"tasks": [
								{"role":"analyst-b1","prompt":"do analyst b1"},
								{"role":"analyst-b2","prompt":"do analyst b2"}
							],
							"wait": "all"
						}`),
					},
					{
						ID:    "call-read-3",
						Name:  "read_file",
						Input: []byte(`{"path":"summary.txt"}`),
					},
				},
			},
			{
				Content: "全部子任务完成，汇总完毕。",
			},
		},
	}
	agent.InjectProvider(prov)

	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()

	done := make(chan struct{})
	go func() {
		_ = agent.Run(runCtx)
		close(done)
	}()

	if err := agent.SendIntent(types.TriggerIntentReceived); err != nil {
		t.Fatalf("SendIntent failed: %v", err)
	}

	// 等待两组 spawn 均执行完成并挂起（共 4 个子任务注册到 Blackboard）
	var childIDs []string
	for i := 0; i < 50; i++ {
		agent.sCtx.Mu.Lock()
		taskIDs := append([]string(nil), agent.sCtx.HandoffTaskIDs...)
		spawnMap := agent.sCtx.LoopHandoffSpawnCallID
		agent.sCtx.Mu.Unlock()

		if len(taskIDs) == 4 && len(spawnMap) == 2 {
			childIDs = taskIDs
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if len(childIDs) != 4 {
		t.Fatalf("expected 4 HandoffTaskIDs across 2 spawn calls, got: %v", childIDs)
	}

	agent.sCtx.Mu.Lock()
	spawnMap := agent.sCtx.LoopHandoffSpawnCallID
	agent.sCtx.Mu.Unlock()

	group1 := spawnMap["call-spawn-1"]
	group2 := spawnMap["call-spawn-2"]
	if len(group1) != 2 {
		t.Fatalf("expected 2 tasks for call-spawn-1, got: %v", group1)
	}
	if len(group2) != 2 {
		t.Fatalf("expected 2 tasks for call-spawn-2, got: %v", group2)
	}

	// 模拟外部 worker 将 4 个子任务全部写为 Done
	taskOutputs := map[string]string{
		group1[0]: "output_a1_success",
		group1[1]: "output_a2_success",
		group2[0]: "output_b1_success",
		group2[1]: "output_b2_success",
	}

	for id, out := range taskOutputs {
		claimed, err := bb.ClaimTask(context.Background(), id, "worker-node")
		if err != nil || !claimed {
			t.Fatalf("ClaimTask %s failed: %v", id, err)
		}
		if err := bb.CompleteTask(context.Background(), id, "worker-node", []byte(out)); err != nil {
			t.Fatalf("CompleteTask %s failed: %v", id, err)
		}
	}

	// 等待恢复执行并完成回合
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for agent to resume and complete loop turn")
	}

	// 断言 LoopMessages 中的 3 条 tool_result 与 3 个 tool_use_id 一一对应
	agent.sCtx.Mu.Lock()
	defer agent.sCtx.Mu.Unlock()

	var toolResultMsgs []types.Message
	for _, m := range agent.sCtx.LoopMessages {
		if m.Role == "user" {
			for _, p := range m.Parts {
				if pMap, ok := p.(map[string]any); ok && pMap["type"] == "tool_result" {
					toolResultMsgs = append(toolResultMsgs, m)
					break
				}
			}
		}
	}

	if len(toolResultMsgs) != 3 {
		t.Fatalf("expected exactly 3 tool_result messages, got %d: %+v", len(toolResultMsgs), toolResultMsgs)
	}

	// 验证 tool_use_id 对应与内容包含关系
	resultsByID := make(map[string]string)
	for _, m := range toolResultMsgs {
		for _, p := range m.Parts {
			if pMap, ok := p.(map[string]any); ok && pMap["type"] == "tool_result" {
				id, _ := pMap["tool_use_id"].(string)
				content, _ := pMap["content"].(string)
				resultsByID[id] = content
			}
		}
	}

	// 1. call-spawn-1 必须包含 a1 和 a2 的输出
	res1, ok1 := resultsByID["call-spawn-1"]
	if !ok1 {
		t.Errorf("missing tool_result for call-spawn-1")
	} else {
		if !strings.Contains(res1, "output_a1_success") || !strings.Contains(res1, "output_a2_success") {
			t.Errorf("call-spawn-1 result does not contain expected subagent outputs: %s", res1)
		}
	}

	// 2. call-spawn-2 必须包含 b1 和 b2 的输出
	res2, ok2 := resultsByID["call-spawn-2"]
	if !ok2 {
		t.Errorf("missing tool_result for call-spawn-2")
	} else {
		if !strings.Contains(res2, "output_b1_success") || !strings.Contains(res2, "output_b2_success") {
			t.Errorf("call-spawn-2 result does not contain expected subagent outputs: %s", res2)
		}
	}

	// 3. call-read-3 必须包含 read_file 的输出
	res3, ok3 := resultsByID["call-read-3"]
	if !ok3 {
		t.Errorf("missing tool_result for call-read-3")
	} else {
		if !strings.Contains(res3, "file content: mock data") {
			t.Errorf("call-read-3 result does not contain file content: %s", res3)
		}
	}

	// 4. HandoffTaskIDs 与 LoopHandoffSpawnCallID 在恢复后必须清空
	if len(agent.sCtx.HandoffTaskIDs) != 0 {
		t.Errorf("expected HandoffTaskIDs to be empty after resume, got: %v", agent.sCtx.HandoffTaskIDs)
	}
	if len(agent.sCtx.LoopHandoffSpawnCallID) != 0 {
		t.Errorf("expected LoopHandoffSpawnCallID to be empty after resume, got: %v", agent.sCtx.LoopHandoffSpawnCallID)
	}
}
