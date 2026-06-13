package server

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

type mockAgentController struct {
	intentSet       []byte
	lastTrigger     protocol.AgentTrigger
	siValue         float64
	sendIntentDelay time.Duration
	sendIntentErr   error
}

func (m *mockAgentController) SetTaskIntent(intent []byte) {
	m.intentSet = intent
}

func (m *mockAgentController) SendIntent(trigger protocol.AgentTrigger) error {
	m.lastTrigger = trigger
	if m.sendIntentDelay > 0 {
		time.Sleep(m.sendIntentDelay)
	}
	return m.sendIntentErr
}

func (m *mockAgentController) SurpriseIndex() float64 {
	return m.siValue
}

func (m *mockAgentController) Memory() protocol.Memory {
	return nil
}

func (m *mockAgentController) Interrupt(req protocol.InterruptRequest) {}

func (m *mockAgentController) AgentID() string { return "test-agent" }

func (m *mockAgentController) CurrentState() protocol.AgentState {
	return protocol.AgentStateIdle
}

func (m *mockAgentController) ConfigInfo() map[string]any {
	return nil
}

func (m *mockAgentController) SetPreferences(prefs map[string]string) {}

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

	err := ctrl.SendIntent(protocol.TriggerIntentReceived)
	if err != nil {
		t.Errorf("Expected nil error, got %v", err)
	}
	if agent.lastTrigger != protocol.TriggerIntentReceived {
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
		sendIntentErr: perrors.New(perrors.CodeInternal, "SendIntent timeout"),
	}

	var ctrl protocol.AgentController = agent
	err := ctrl.SendIntent(protocol.TriggerIntentReceived)
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

	_, err = db.Exec(`CREATE TABLE skills (name TEXT, instructions TEXT, exec_mode TEXT, deprecated INTEGER, trust_tier INTEGER)`)
	if err != nil {
		t.Fatal(err)
	}

	srv := &Server{db: db}

	// 插入两条 ambient skill
	db.Exec(`INSERT INTO skills (name, instructions, exec_mode, deprecated, trust_tier) VALUES ('skill1', 'inst1', 'ambient', 0, 1)`)
	db.Exec(`INSERT INTO skills (name, instructions, exec_mode, deprecated, trust_tier) VALUES ('skill4', 'inst4', 'ambient', 0, 4)`)

	result := srv.buildAmbientSkillsSection()
	if !strings.Contains(result, "skill4") {
		t.Fatal("missing skill4")
	}
	idx1 := strings.Index(result, "skill4")
	idx2 := strings.Index(result, "skill1")
	if idx1 > idx2 {
		t.Fatalf("tier 4 should be before tier 1")
	}

	// 插入超长技能组合
	db.Exec(`INSERT INTO skills (name, instructions, exec_mode, deprecated, trust_tier) VALUES ('skill6', ?, 'ambient', 0, 6)`, strings.Repeat("B", 2000))
	db.Exec(`INSERT INTO skills (name, instructions, exec_mode, deprecated, trust_tier) VALUES ('skill5', ?, 'ambient', 0, 5)`, strings.Repeat("C", 2500))

	result2 := srv.buildAmbientSkillsSection()
	if len(result2) > 4100 { // \n\n 等有额外开销
		t.Fatalf("result too long: %d", len(result2))
	}
	if !strings.Contains(result2, "skill6") {
		t.Fatal("missing skill6 which has highest tier and fits")
	}
	if strings.Contains(result2, "skill5") {
		t.Fatal("skill5 should be truncated (dropped entirely)")
	}
}
