package session

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/pkg/types"
)

type fakeGoalStore struct {
	mu     sync.Mutex
	g      repo.Goal
	idle   []bool
	paused bool
}

func (f *fakeGoalStore) ActiveGoal(context.Context, string) (*repo.Goal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.g.Status != repo.GoalActive {
		return nil, nil
	}
	g := f.g
	return &g, nil
}

func (f *fakeGoalStore) BeginRound(context.Context, string) (*repo.Goal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.g.RoundsUsed++
	g := f.g
	return &g, nil
}

func (f *fakeGoalStore) FinishRound(_ context.Context, _ string, idle bool, _ int) (*repo.Goal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.idle = append(f.idle, idle)
	g := f.g
	return &g, nil
}

func (f *fakeGoalStore) PauseGoal(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paused = true
	f.g.Status = repo.GoalPaused
	return nil
}

func tokenTurn(ctrl *fakeAgentController, text string) {
	ctrl.events <- types.AgentStreamEvent{Type: types.AgentStreamEventToken, Content: text}
	ctrl.events <- types.AgentStreamEvent{Type: types.AgentStreamEventStatus, Content: "task_done"}
}

// 活跃目标 + 回合正常结束 → 自动续跑，直到轮次耗尽后暂停；续跑指令带轮次与目标。
func TestRunTurn_Interactive_GoalContinuesUntilRoundsExhausted(t *testing.T) {
	ctrl := newFakeAgentController()
	go func() {
		tokenTurn(ctrl, "r1")
		tokenTurn(ctrl, "r2")
		tokenTurn(ctrl, "r3")
	}()
	goals := &fakeGoalStore{g: repo.Goal{ID: "g1", Objective: "迁移数据库", DoneCriteria: "测试全绿", Status: repo.GoalActive, MaxRounds: 2}}
	orc := newTestOrchestrator(t, newFakePersistence(), &fakeHooks{}, &fakeSlash{}, &fakeCompression{}, &fakeAgentPool{ctrl: ctrl})
	orc.(*orchestrator).goals = goals

	res, err := orc.RunTurn(context.Background(), Request{SessionID: "s1", Input: "开始", Channel: "web"}, &recordingSink{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reply != "r3" {
		t.Fatalf("reply=%q", res.Reply)
	}
	if len(ctrl.intents) != 3 || !strings.Contains(ctrl.intents[1], "第 1/2 轮") ||
		!strings.Contains(ctrl.intents[2], "第 2/2 轮") || !strings.Contains(ctrl.intents[1], "迁移数据库") ||
		!strings.Contains(ctrl.intents[1], "测试全绿") {
		t.Fatalf("intents=%q", ctrl.intents)
	}
	if !goals.paused {
		t.Fatal("轮次耗尽后应暂停目标")
	}
	if len(goals.idle) != 2 || !goals.idle[0] || !goals.idle[1] {
		t.Fatalf("零工具调用的续跑应记为空转: %v", goals.idle)
	}
}

// 没有 active 目标时不续跑。
func TestRunTurn_Interactive_NoGoalNoContinuation(t *testing.T) {
	ctrl := newFakeAgentController()
	go tokenTurn(ctrl, "only")
	goals := &fakeGoalStore{g: repo.Goal{ID: "g1", Status: repo.GoalComplete, MaxRounds: 5}}
	orc := newTestOrchestrator(t, newFakePersistence(), &fakeHooks{}, &fakeSlash{}, &fakeCompression{}, &fakeAgentPool{ctrl: ctrl})
	orc.(*orchestrator).goals = goals
	if _, err := orc.RunTurn(context.Background(), Request{SessionID: "s1", Input: "hi", Channel: "web"}, &recordingSink{}); err != nil {
		t.Fatal(err)
	}
	if len(ctrl.intents) != 1 {
		t.Fatalf("intents=%q", ctrl.intents)
	}
}
