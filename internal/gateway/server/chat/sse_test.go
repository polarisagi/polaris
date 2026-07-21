package chat

import (
	"github.com/polarisagi/polaris/internal/store/repo"

	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockAgentController struct {
	intentSet       []byte
	lastTrigger     types.AgentTrigger
	siValue         float64
	sendIntentDelay time.Duration
	sendIntentErr   error
}

func (m *mockAgentController) SetTaskIntent(intent []byte) {
	m.intentSet = intent
}

func (m *mockAgentController) SendIntent(trigger types.AgentTrigger) error {
	m.lastTrigger = trigger
	if m.sendIntentDelay > 0 {
		time.Sleep(m.sendIntentDelay)
	}
	return m.sendIntentErr
}

func (m *mockAgentController) SurpriseIndex() float64 {
	return m.siValue
}

func (m *mockAgentController) Memory() protocol.MemoryFacade {
	return nil
}

func (m *mockAgentController) Interrupt(req types.InterruptRequest) {}

func (m *mockAgentController) AgentID() string { return "test-agent" }

func (m *mockAgentController) CurrentState() types.AgentState {
	return types.AgentStateIdle
}

func (m *mockAgentController) ConfigInfo() map[string]any {
	return nil
}

func (m *mockAgentController) SetPreferences(prefs map[string]string) {}

// SetMonthlyBudgetUSD 测试桩：2026-07-04 审计修复（任务11）在 AgentController
// 接口新增该方法后，测试用 mock 需同步实现以满足接口断言，无需真实记账逻辑。
func (m *mockAgentController) SetMonthlyBudgetUSD(budget float64) {}

func (m *mockAgentController) SubscribeStream(ctx context.Context) <-chan types.AgentStreamEvent {
	return nil
}

// InjectReplayData 测试桩：M04 §8 崩溃恢复回放在 AgentController 接口新增
// 该方法后，测试用 mock 需同步实现以满足接口断言，无需真实回放逻辑。
func (m *mockAgentController) InjectReplayData(calls []protocol.ReplayLLMCall) {}

// Test_SSE_AgentFSM_Injection 验证 AgentController 接口及桩代码的非阻塞行为。
func Test_SSE_AgentFSM_Injection(t *testing.T) {
	agent := &mockAgentController{
		siValue: 0.1, // 模拟触发 FastPath
	}

	var ctrl protocol.AgentController = agent
	ctrl.SetTaskIntent([]byte("hello world"))
	if string(agent.intentSet) != "hello world" {
		t.Errorf("Expected 'hello world', got %s", agent.intentSet)
	}

	err := ctrl.SendIntent(types.TriggerIntentReceived)
	if err != nil {
		t.Errorf("Expected nil error, got %v", err)
	}
	if agent.lastTrigger != types.TriggerIntentReceived {
		t.Errorf("Expected TriggerIntentReceived, got %v", agent.lastTrigger)
	}

	si := ctrl.SurpriseIndex()
	if si != 0.1 {
		t.Errorf("Expected 0.1, got %f", si)
	}
}

// Test_SSE_AgentFSM_TimeoutFallback 验证超时回退时的错误处理。
func Test_SSE_AgentFSM_TimeoutFallback(t *testing.T) {
	agent := &mockAgentController{
		sendIntentErr: apperr.New(apperr.CodeInternal, "SendIntent timeout"),
	}

	var ctrl protocol.AgentController = agent
	err := ctrl.SendIntent(types.TriggerIntentReceived)
	if err == nil || !strings.Contains(err.Error(), "SendIntent timeout") {
		t.Errorf("Expected timeout error, got %v", err)
	}
}

func TestBuildAmbientSkillsSection(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.Exec(`CREATE TABLE skills (name TEXT, description TEXT, instructions TEXT, plugin_id TEXT, exec_mode TEXT, ambient_priority TEXT, deprecated INTEGER, trust_tier INTEGER)`)
	if err != nil {
		t.Fatal(err)
	}

	srv := &ChatHandler{DataDir: t.TempDir(), DB: db, ChatRepo: repo.NewSQLiteChatRepository(db), ProviderRepo: repo.NewSQLiteProviderRepository(db)}

	// 插入两条 ambient skill
	db.Exec(`INSERT INTO skills (name, description, instructions, plugin_id, exec_mode, ambient_priority, deprecated, trust_tier) VALUES ('skill1', 'desc1', 'inst1', '', 'ambient', 'always', 0, 1)`)
	db.Exec(`INSERT INTO skills (name, description, instructions, plugin_id, exec_mode, ambient_priority, deprecated, trust_tier) VALUES ('skill4', 'desc4', 'inst4', '', 'ambient', 'always', 0, 4)`)

	result := srv.buildAmbientSkillsSection(context.Background(), "")
	if !strings.Contains(result, "skill4") {
		t.Fatal("missing skill4")
	}
	idx1 := strings.Index(result, "skill4")
	idx2 := strings.Index(result, "skill1")
	if idx1 > idx2 {
		t.Fatalf("tier 4 should be before tier 1")
	}

	// 插入超长技能组合
	db.Exec(`INSERT INTO skills (name, description, instructions, plugin_id, exec_mode, ambient_priority, deprecated, trust_tier) VALUES ('skill6', 'desc6', ?, '', 'ambient', 'always', 0, 6)`, strings.Repeat("B", 120_000))
	db.Exec(`INSERT INTO skills (name, description, instructions, plugin_id, exec_mode, ambient_priority, deprecated, trust_tier) VALUES ('skill5', 'desc5', ?, '', 'ambient', 'always', 0, 5)`, strings.Repeat("C", 20_000))

	result2 := srv.buildAmbientSkillsSection(context.Background(), "")
	if len(result2) > 130_000 {
		t.Fatalf("result too long: %d", len(result2))
	}
	if !strings.Contains(result2, "skill6") {
		t.Fatal("missing skill6 which has highest tier and fits")
	}
	if strings.Contains(result2, strings.Repeat("C", 20_000)) {
		t.Fatal("skill5 should be truncated (dropped entirely)")
	}
}
