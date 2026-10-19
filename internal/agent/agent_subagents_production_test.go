package agent

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/polarisagi/polaris/internal/execute/orchestrator"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/pkg/types"
)

type subagentTestProvider struct {
	gate chan struct{}
}

func (p *subagentTestProvider) Infer(_ context.Context, _ []types.Message, _ ...types.InferOption) (*types.ProviderResponse, error) {
	return &types.ProviderResponse{Content: "test"}, nil
}

func (p *subagentTestProvider) StreamInfer(ctx context.Context, _ []types.Message, _ ...types.InferOption) (<-chan types.StreamEvent, error) {
	ch := make(chan types.StreamEvent, 5)
	ch <- types.StreamEvent{Type: types.StreamTextDelta, Content: "working"}
	go func() {
		defer close(ch)
		select {
		case <-p.gate:
		case <-ctx.Done():
		}
	}()
	return ch, nil
}

func (p *subagentTestProvider) Capabilities() types.ProviderCapabilities {
	return types.ProviderCapabilities{SupportsStreaming: true}
}
func (p *subagentTestProvider) Tokenizer() protocol.TokenizerAdapter { return nil }
func (p *subagentTestProvider) ModelID() string                      { return "subagent-test" }

func setupTestDBWithBlackboardSchema(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	for _, f := range []string{"001_events.sql", "007_tasks.sql", "035_task_checkpoints.sql"} {
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

// TestSubagentControl_ProductionAssembly 验证生产装配下：
// 1. 通用角色子智能体运行中收到插话，其 LoopMessages/LoopSteerQueue 包含该文本；
// 2. 子智能体的事件总数有界，没有回环（插话一次，SteerAccepted 恰好出现 1 次）；
// 3. 中止后子 Agent 的 ctx 被取消，Pool 中不再持有该会话 (R3-04)。
func TestSubagentControl_ProductionAssembly(t *testing.T) {
	t.Setenv("POLARIS_LOOP_MODE", "agentic")

	db := setupTestDBWithBlackboardSchema(t)
	bb := orchestrator.NewSQLiteBlackboard(db)

	var activeAgentMu sync.Mutex
	activeAgents := make(map[string]*Agent)
	activeProviders := make(map[string]*subagentTestProvider)

	pool := NewPool(func(sessionID string) *Agent {
		ag := NewAgentWithDefaults(sessionID)
		prov := &subagentTestProvider{gate: make(chan struct{})}
		ag.InjectProvider(prov)

		activeAgentMu.Lock()
		activeAgents[sessionID] = ag
		activeProviders[sessionID] = prov
		activeAgentMu.Unlock()
		return ag
	}, 10)

	worker := orchestrator.NewDefaultTaskWorker(bb, pool)

	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer func() {
		workerCancel()
		time.Sleep(20 * time.Millisecond)
	}()
	go func() { _ = worker.RunLoop(workerCtx) }()

	t.Run("steer and loop elimination", func(t *testing.T) {
		taskID := "task-steer-prod-1"
		task := &types.TaskEntry{
			ID:     taskID,
			Type:   "agent_handoff:general-purpose",
			Status: types.TaskPending,
			Intent: []byte(`{"prompt": "analyze data"}`),
		}

		eventCh, cancelEventSub := bb.SubscribeTaskEvents(taskID)
		defer cancelEventSub()

		var steerAcceptedCount int32
		go func() {
			for ev := range eventCh {
				if ev.Type == types.AgentStreamEventSteerAccepted {
					atomic.AddInt32(&steerAcceptedCount, 1)
				}
			}
		}()

		if err := bb.PostTask(context.Background(), task); err != nil {
			t.Fatalf("PostTask failed: %v", err)
		}

		targetSessionID := "sub-" + taskID
		var subAgent *Agent
		var prov *subagentTestProvider
		for i := 0; i < 50; i++ {
			activeAgentMu.Lock()
			subAgent = activeAgents[targetSessionID]
			prov = activeProviders[targetSessionID]
			activeAgentMu.Unlock()
			if subAgent != nil && subAgent.sm.Current() == types.AgentStateLoop {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if subAgent == nil {
			t.Fatalf("expected sub-agent with session %s to be created in pool", targetSessionID)
		}
		defer close(prov.gate)

		steerText := "请优先检查边界用例"
		delivered := bb.SendTaskControl(taskID, types.TaskControl{
			Kind: types.TaskControlSteer,
			Text: steerText,
		})
		if !delivered {
			t.Fatalf("SendTaskControl steer returned false, expected true")
		}

		// 等待 steer 进入队列
		var foundSteer bool
		for i := 0; i < 20; i++ {
			subAgent.sCtx.Mu.Lock()
			for _, msg := range subAgent.sCtx.LoopSteerQueue {
				if msg.UnsafeContent() == steerText {
					foundSteer = true
					break
				}
			}
			subAgent.sCtx.Mu.Unlock()
			if foundSteer {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !foundSteer {
			t.Errorf("subAgent LoopSteerQueue does not contain %q", steerText)
		}

		// 验证无回环：插话 1 次，SteerAccepted 恰好 1 次
		time.Sleep(150 * time.Millisecond)
		count := atomic.LoadInt32(&steerAcceptedCount)
		if count != 1 {
			t.Errorf("expected SteerAccepted to appear exactly 1 time (no feedback loop), got %d", count)
		}
	})

	t.Run("cancel and pool release", func(t *testing.T) {
		taskID := "task-cancel-prod-2"
		task := &types.TaskEntry{
			ID:     taskID,
			Type:   "agent_handoff:general-purpose",
			Status: types.TaskPending,
			Intent: []byte(`{"prompt": "long running task"}`),
		}

		if err := bb.PostTask(context.Background(), task); err != nil {
			t.Fatalf("PostTask failed: %v", err)
		}

		targetSessionID := "sub-" + taskID
		var subAgent *Agent
		for i := 0; i < 50; i++ {
			activeAgentMu.Lock()
			subAgent = activeAgents[targetSessionID]
			activeAgentMu.Unlock()
			if subAgent != nil && subAgent.sm.Current() == types.AgentStateLoop {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if subAgent == nil {
			t.Fatalf("expected sub-agent %s in pool", targetSessionID)
		}

		delivered := bb.SendTaskControl(taskID, types.TaskControl{
			Kind: types.TaskControlCancel,
		})
		if !delivered {
			t.Fatalf("SendTaskControl cancel returned false, expected true")
		}

		// 等待取消生效：断言 subAgent 的 Done() 被关闭
		select {
		case <-subAgent.Done():
		case <-time.After(1 * time.Second):
			t.Errorf("expected subAgent context/done to be cancelled after AgentStop")
		}

		// Pool 中该 session 已被释放（TrySteer 无法命中）
		if pool.TrySteer(targetSessionID, "test after cancel") {
			t.Errorf("pool should no longer hold session %s after cancel", targetSessionID)
		}
	})
}
