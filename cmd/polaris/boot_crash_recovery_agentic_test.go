package main

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/schema"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// TestCrashRecovery_AgenticLoop_EndToEnd 验证 boot_crash_recovery 中 agentic 循环增量崩溃恢复 (R2-05 / R3-06)。
func TestCrashRecovery_AgenticLoop_EndToEnd(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	ddl, err := schema.FS.ReadFile("051_agent_loop_rounds.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}

	sessionID := "sess-agentic-crash"
	turnID := "turn-agentic-99"

	// 插入第 1 轮（用户提问 + assistant tool_use，工具执行中断未完成）
	round1Delta := `[{"Role":"user","Content":"list files","Round":1},{"Role":"assistant","Content":"calling tool","Round":1,"Parts":[{"type":"tool_use","id":"call-1","name":"ls","input":{}}]}]`
	_, err = db.Exec(`
		INSERT INTO agent_loop_rounds (session_id, turn_id, round, status, delta_json, taint_level, created_at)
		VALUES (?, ?, 1, 'running', ?, 0, 1000)`, sessionID, turnID, round1Delta)
	if err != nil {
		t.Fatal(err)
	}

	ctrl := newFakeAgentController()
	pool := &fakeAgentPool{ctrl: ctrl}

	recoverAgenticLoopTurns(context.Background(), db, pool)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !ctrl.isResumeCalled() {
		time.Sleep(10 * time.Millisecond)
	}

	if !ctrl.isResumeCalled() {
		t.Fatal("expected ResumeAgenticLoop to be called")
	}
	resumedTurnID, resumedMessages := ctrl.getResumedData()
	if resumedTurnID != turnID {
		t.Fatalf("expected turnID %q, got %q", turnID, resumedTurnID)
	}
	// 验证未完成的 call-1 被自动补齐了中断消息
	foundInterrupted := false
	for _, m := range resumedMessages {
		if strings.Contains(m.Content, "执行中断，结果未知，请自行检查后决定是否重试") {
			foundInterrupted = true
			break
		}
	}
	if !foundInterrupted {
		t.Fatalf("expected interrupted tool_result in resumed messages: %+v", resumedMessages)
	}

	// 标记该回合已完成
	_, _ = db.Exec(`UPDATE agent_loop_rounds SET status = 'done' WHERE session_id = ? AND turn_id = ?`, sessionID, turnID)

	// 再次调用 recoverAgenticLoopTurns，断言已完成的回合不会被重放
	ctrl2 := newFakeAgentController()
	pool2 := &fakeAgentPool{ctrl: ctrl2}
	recoverAgenticLoopTurns(context.Background(), db, pool2)
	time.Sleep(50 * time.Millisecond)
	if ctrl2.isResumeCalled() {
		t.Fatal("done turn should not be recovered again")
	}
}

type trackingAgentCtrl struct {
	pool      *trackingAgentPool
	sessionID string
	streamCh  chan types.AgentStreamEvent
}

func (c *trackingAgentCtrl) AgentID() string                             { return c.sessionID }
func (c *trackingAgentCtrl) SetTaskIntent(_ taint.TaintedString)         {}
func (c *trackingAgentCtrl) SetSpawnDepth(_ int)                         {}
func (c *trackingAgentCtrl) SetMemoryNamespace(_ string)                 {}
func (c *trackingAgentCtrl) SetAgentProfile(_ *types.AgentProfileSpec)   {}
func (c *trackingAgentCtrl) SurpriseIndex() float64                      { return 0 }
func (c *trackingAgentCtrl) Memory() protocol.MemoryFacade               { return nil }
func (c *trackingAgentCtrl) Interrupt(_ types.InterruptRequest)          {}
func (c *trackingAgentCtrl) SetPreferences(_ map[string]string)          {}
func (c *trackingAgentCtrl) CurrentState() types.AgentState              { return types.AgentStateComplete }
func (c *trackingAgentCtrl) ConfigInfo() map[string]any                  { return nil }
func (c *trackingAgentCtrl) InjectReplayData(_ []protocol.ReplayLLMCall) {}
func (c *trackingAgentCtrl) SetConversationHistory(_ []types.Message)    {}
func (c *trackingAgentCtrl) Steer(_ string) bool                         { return true }
func (c *trackingAgentCtrl) SetModelLock(_ string)                       {}
func (c *trackingAgentCtrl) SubscribeStream(_ context.Context) <-chan types.AgentStreamEvent {
	return c.streamCh
}
func (c *trackingAgentCtrl) SendIntent(_ types.AgentTrigger) error {
	close(c.streamCh)
	return nil
}
func (c *trackingAgentCtrl) ResumeAgenticLoop(_ string, _ []types.Message, _ types.TaintLevel, _ int) bool {
	c.pool.mu.Lock()
	c.pool.active++
	if c.pool.active > c.pool.maxActive {
		c.pool.maxActive = c.pool.active
	}
	c.pool.mu.Unlock()

	// 模拟异步恢复执行耗时
	time.Sleep(50 * time.Millisecond)

	c.pool.mu.Lock()
	c.pool.active--
	c.pool.completed++
	if c.pool.completed == c.pool.expectedTotal {
		close(c.pool.doneCh)
	}
	c.pool.mu.Unlock()

	close(c.streamCh)
	return true
}

type trackingAgentPool struct {
	mu            sync.Mutex
	active        int
	maxActive     int
	completed     int
	expectedTotal int
	doneCh        chan struct{}
}

func (p *trackingAgentPool) Acquire(_ context.Context, sessionID string) (protocol.AgentController, func(), error) {
	ctrl := &trackingAgentCtrl{
		pool:      p,
		sessionID: sessionID,
		streamCh:  make(chan types.AgentStreamEvent),
	}
	return ctrl, func() {}, nil
}

func (p *trackingAgentPool) AcquireHeadless(_ context.Context, _ types.Intent, _ ...types.HeadlessOption) (*types.AgentResult, error) {
	return nil, nil
}

// TestRecoverAgenticLoopTurns_NonBlocking 验证崩溃恢复在有 3 个未完成回合时不阻塞主路径且后台受限并发 ≤ 2 (R3-06)。
func TestRecoverAgenticLoopTurns_NonBlocking(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	ddl, err := schema.FS.ReadFile("051_agent_loop_rounds.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}

	// 插入 3 个 running 回合
	deltaJSON := `[{"Role":"user","Content":"hi","Round":1}]`
	for i := 1; i <= 3; i++ {
		sessID := "sess-" + string(rune('0'+i))
		turnID := "turn-" + string(rune('0'+i))
		_, err = db.Exec(`
			INSERT INTO agent_loop_rounds (session_id, turn_id, round, status, delta_json, taint_level, created_at)
			VALUES (?, ?, 1, 'running', ?, 0, 1000)`, sessID, turnID, deltaJSON)
		if err != nil {
			t.Fatal(err)
		}
	}

	pool := &trackingAgentPool{
		expectedTotal: 3,
		doneCh:        make(chan struct{}),
	}

	start := time.Now()
	recoverAgenticLoopTurns(context.Background(), db, pool)
	elapsed := time.Since(start)

	// 断言启动函数非阻塞立即返回（< 1s）
	if elapsed > 1*time.Second {
		t.Fatalf("recoverAgenticLoopTurns took too long (%v), expected non-blocking < 1s", elapsed)
	}

	// 等待后台恢复任务全部完成
	select {
	case <-pool.doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for background recovery turns to complete")
	}

	pool.mu.Lock()
	defer pool.mu.Unlock()

	if pool.completed != 3 {
		t.Fatalf("expected 3 completed turns, got %d", pool.completed)
	}
	// 断言后台受限并发 ≤ 2
	if pool.maxActive > 2 {
		t.Fatalf("expected max concurrency <= 2, got %d", pool.maxActive)
	}
}
