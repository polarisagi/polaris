package agent

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/pkg/types"
)

func newTestLoopRoundsDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite failed: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	ddl, err := schema.FS.ReadFile("051_agent_loop_rounds.sql")
	if err != nil {
		t.Fatalf("read 051_agent_loop_rounds.sql failed: %v", err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatalf("exec ddl failed: %v", err)
	}
	return db
}

// recordingToolExecutor 记录执行过的工具名称及输入。
type recordingToolExecutor struct {
	mu        sync.Mutex
	executed  []string
	onExecute func(name string)
}

func (r *recordingToolExecutor) Lookup(name string) (types.Tool, error) {
	return types.Tool{Name: name, Source: types.ToolBuiltin, Capability: types.CapReadOnly}, nil
}

func (r *recordingToolExecutor) ExecuteWithTaint(_ context.Context, name string, _ []byte, _ types.TaintLevel) (*types.ToolResult, error) {
	r.mu.Lock()
	r.executed = append(r.executed, name)
	cb := r.onExecute
	r.mu.Unlock()
	if cb != nil {
		cb(name)
	}
	return &types.ToolResult{Success: true, Output: []byte(fmt.Sprintf(`{"executed":%q}`, name))}, nil
}

func (r *recordingToolExecutor) getExecuted() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.executed))
	copy(out, r.executed)
	return out
}

// TestAgenticLoop_CrashRecovery_MidToolCall_NoReplay 验证 R2-05：
// 最后一轮如果是「assistant tool_use 已写入、tool_result 未写入」，不自动重放工具，
// 而是为这些调用回填 tool_result「执行中断，结果未知，请自行检查后决定是否重试」，交给模型判断。
func TestAgenticLoop_CrashRecovery_MidToolCall_NoReplay(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")
	sessionID := "test-session-crash-midtool"
	db := newTestLoopRoundsDB(t)

	// --- 阶段一：运行 Agent 1 执行第 1 轮工具调用，在第 2 轮生成 tool_use 后模拟崩溃 ---
	var cancel1 context.CancelFunc
	ctx, cancel1 := context.WithCancel(context.Background())
	defer cancel1()

	exec1 := &recordingToolExecutor{
		onExecute: func(name string) {
			if name == "echo_2" {
				cancel1() // 模拟在第 2 轮工具执行中崩溃
				time.Sleep(500 * time.Millisecond)
			}
		},
	}
	prov1 := &loopTestProvider{
		responses: []*types.ProviderResponse{
			// 第 1 轮：调用 echo_1
			{ToolCalls: []types.InferToolCall{{ID: "c1", Name: "echo_1", Input: []byte(`{}`)}}},
			// 第 2 轮：调用 echo_2（但在工具执行中崩溃）
			{ToolCalls: []types.InferToolCall{{ID: "c2", Name: "echo_2", Input: []byte(`{}`)}}},
		},
	}

	agent1 := NewAgentWithDefaults(sessionID)
	agent1.SetExtQuerier(db)
	agent1.InjectProvider(prov1)
	agent1.InjectPolicyGate(&allowPolicyGate{})
	agent1.InjectToolExecutor(exec1)

	done1 := make(chan struct{})
	go func() {
		_ = agent1.Run(ctx)
		close(done1)
	}()

	if err := agent1.SendIntent(types.TriggerIntentReceived); err != nil {
		t.Fatalf("agent1 SendIntent 失败: %v", err)
	}

	// 等待 agent 1 崩溃退出
	select {
	case <-done1:
	case <-time.After(5 * time.Second):
		t.Fatal("agent 1 timeout waiting for crash")
	}

	turnID := agent1.sCtx.TurnID
	if turnID == "" {
		t.Fatal("turnID should not be empty")
	}

	// --- 阶段二：进程重启，从 agent_loop_rounds 恢复续跑 ---
	messages, maxTaint, maxRound, err := ReconstructAgenticLoopTurn(context.Background(), db, sessionID, turnID)
	if err != nil {
		t.Fatalf("ReconstructAgenticLoopTurn failed: %v", err)
	}

	// 断言：最后一轮的未完成 tool_use c2 被自动回填了中断提示，且没有自动重新执行 c2！
	lastMsg := messages[len(messages)-1]
	if lastMsg.Role != "user" || !strings.Contains(lastMsg.Content, "执行中断，结果未知，请自行检查后决定是否重试") {
		t.Fatalf("expected interrupted tool_result in last message, got: %+v", lastMsg)
	}

	exec2 := &recordingToolExecutor{}
	prov2 := &loopTestProvider{
		responses: []*types.ProviderResponse{
			// 第 3 轮：模型收到中断结果后输出总结并完成
			{Content: "检测到前序工具调用中断，已安全收尾。"},
		},
	}

	agent2 := NewAgentWithDefaults(sessionID)
	agent2.SetExtQuerier(db)
	agent2.InjectProvider(prov2)
	agent2.InjectPolicyGate(&allowPolicyGate{})
	agent2.InjectToolExecutor(exec2)

	done2 := make(chan struct{})
	go func() {
		_ = agent2.Run(context.Background())
		close(done2)
	}()

	restored := agent2.ResumeAgenticLoop(turnID, messages, maxTaint, maxRound)
	if !restored {
		t.Fatal("agent2.ResumeAgenticLoop failed")
	}

	select {
	case <-done2:
	case <-time.After(5 * time.Second):
		t.Fatal("agent 2 timeout")
	}

	// 断言：工具未被盲目重放，exec2 没有任何工具执行
	if len(exec2.getExecuted()) != 0 {
		t.Fatalf("expected no tool replayed blindly, got: %v", exec2.getExecuted())
	}

	// 断言：回合完成时整个 turn 状态被更新为 done
	var turnStatus string
	if err := db.QueryRow("SELECT status FROM agent_loop_rounds WHERE session_id = ? AND turn_id = ? LIMIT 1", sessionID, turnID).Scan(&turnStatus); err != nil {
		t.Fatalf("query turn status failed: %v", err)
	}
	if turnStatus != "done" {
		t.Fatalf("expected turn status 'done', got %q", turnStatus)
	}
}

// TestAgenticLoop_CrashRecovery_DoneTurnNotReplayed 验证已完成的回合绝不被重放（R2-05）。
func TestAgenticLoop_CrashRecovery_DoneTurnNotReplayed(t *testing.T) {
	db := newTestLoopRoundsDB(t)
	sessionID := "test-session-done"
	turnID := "turn-completed-123"

	// 插入一个 status='done' 的回合
	_, err := db.Exec(`
		INSERT INTO agent_loop_rounds (session_id, turn_id, round, status, delta_json, taint_level, created_at)
		VALUES (?, ?, 1, 'done', '[]', 0, 1000)`, sessionID, turnID)
	if err != nil {
		t.Fatal(err)
	}

	// 尝试恢复
	_, _, _, recErr := ReconstructAgenticLoopTurn(context.Background(), db, sessionID, turnID)
	if recErr == nil {
		t.Fatal("expected error when reconstructing turn with status='done', but succeeded")
	}

	// 验证 running 查询无结果
	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM agent_loop_rounds WHERE status = 'running'").Scan(&count)
	if count != 0 {
		t.Fatalf("expected 0 running turns, got %d", count)
	}
}

// TestAgenticLoop_200Rounds_NoSizeLimitDrop 验证 200 轮 × 每轮 10KB 的回合能完整持久化，没有任何大小上限导致的丢失（R2-05）。
func TestAgenticLoop_200Rounds_NoSizeLimitDrop(t *testing.T) {
	db := newTestLoopRoundsDB(t)
	sessionID := "test-session-200-rounds"
	turnID := "turn-huge-200"

	payload10KB := strings.Repeat("x", 10*1024)
	agent := NewAgentWithDefaults(sessionID)
	agent.SetExtQuerier(db)
	agent.sCtx.SessionID = sessionID
	agent.sCtx.TurnID = turnID

	// 模拟写入 200 轮，每轮 10KB
	for r := 1; r <= 200; r++ {
		agent.sCtx.LoopRound = r
		agent.appendLoopMessages(types.Message{
			Role:    "assistant",
			Content: fmt.Sprintf("round %d %s", r, payload10KB),
			Round:   r,
		})
		agent.persistLoopRoundDelta(context.Background())
	}

	// 验证 200 轮在数据库中均完整存在
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM agent_loop_rounds WHERE session_id = ? AND turn_id = ?", sessionID, turnID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 200 {
		t.Fatalf("expected 200 persisted rounds, got %d", count)
	}

	// 验证还原后消息条数与内容完全一致，没有被 256KB 截断
	messages, _, maxRound, err := ReconstructAgenticLoopTurn(context.Background(), db, sessionID, turnID)
	if err != nil {
		t.Fatalf("ReconstructAgenticLoopTurn failed: %v", err)
	}
	if maxRound != 200 {
		t.Fatalf("expected maxRound 200, got %d", maxRound)
	}
	if len(messages) != 200 {
		t.Fatalf("expected 200 messages, got %d", len(messages))
	}
	for i, m := range messages {
		expectedPrefix := fmt.Sprintf("round %d", i+1)
		if !strings.HasPrefix(m.Content, expectedPrefix) {
			t.Fatalf("message %d content mismatch", i+1)
		}
	}
}

// TestAgenticLoop_SecretGet_RedactedOnDisk 验证数据卫生：secret_get 明文在落盘前被脱敏（R2-05）。
func TestAgenticLoop_SecretGet_RedactedOnDisk(t *testing.T) {
	db := newTestLoopRoundsDB(t)
	sessionID := "test-session-secret"
	turnID := "turn-secret-1"

	agent := NewAgentWithDefaults(sessionID)
	agent.SetExtQuerier(db)
	agent.sCtx.SessionID = sessionID
	agent.sCtx.TurnID = turnID
	agent.sCtx.LoopRound = 1

	// 构造 assistant 包含 secret_get tool_use，以及 user tool_result 包含明文密文
	plainSecret := "super_secret_api_key_12345"
	callID := "call_sec_99"
	agent.sCtx.LoopMessages = []types.Message{
		{
			Role:    "assistant",
			Content: "正在读取凭据...",
			Round:   1,
			Parts: []any{
				map[string]any{
					"type":  "tool_use",
					"id":    callID,
					"name":  "secret_get",
					"input": map[string]any{"name": "openai_key"},
				},
			},
		},
		{
			Role:    "user",
			Content: plainSecret,
			Round:   1,
			Parts: []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": callID,
					"content":     plainSecret,
				},
			},
		},
	}

	agent.persistLoopRoundDelta(context.Background())

	// 1. 验证内存中的 LoopMessages 仍保留明文供当前会话消费
	if agent.sCtx.LoopMessages[1].Content != plainSecret {
		t.Fatalf("memory LoopMessages should keep plaintext for active turn, got: %s", agent.sCtx.LoopMessages[1].Content)
	}

	// 2. 验证落盘到 DB 中的 delta_json 已经将明文替换为 [secret:openai_key redacted]
	var deltaJSON string
	if err := db.QueryRow("SELECT delta_json FROM agent_loop_rounds WHERE session_id = ? AND turn_id = ? AND round = 1", sessionID, turnID).Scan(&deltaJSON); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(deltaJSON, plainSecret) {
		t.Fatalf("SECURITY VIOLATION: plaintext secret found in persisted delta_json: %s", deltaJSON)
	}
	if !strings.Contains(deltaJSON, "[secret:openai_key redacted]") {
		t.Fatalf("expected '[secret:openai_key redacted]' in persisted delta_json, got: %s", deltaJSON)
	}
}
