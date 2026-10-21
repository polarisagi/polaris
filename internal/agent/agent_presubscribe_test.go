package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// racePoster 模拟黑板语义：PublishTaskEvent 对无订阅者的任务直接丢弃；
// PostTask 同步产生 eventsPerTask 条子事件并置终态，复现"投递快于订阅"。
type racePoster struct {
	mu            sync.Mutex
	tasks         map[string]*types.TaskSnapshot
	subs          map[string][]chan types.AgentStreamEvent
	activeSubs    int
	eventsPerTask int
	postErr       error
	bbCh          chan types.BlackboardEvent
	postedOrder   []string
}

func newRacePoster(n int) *racePoster {
	return &racePoster{
		tasks:         map[string]*types.TaskSnapshot{},
		subs:          map[string][]chan types.AgentStreamEvent{},
		eventsPerTask: n,
		bbCh:          make(chan types.BlackboardEvent, 8),
	}
}

func (p *racePoster) publishLocked(id string, ev types.AgentStreamEvent) {
	for _, ch := range p.subs[id] {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (p *racePoster) PostTask(_ context.Context, t *types.TaskEntry) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.postErr != nil {
		return p.postErr
	}
	p.postedOrder = append(p.postedOrder, t.ID)
	for i := 0; i < p.eventsPerTask; i++ {
		p.publishLocked(t.ID, types.AgentStreamEvent{Type: types.AgentStreamEventStatus, Content: fmt.Sprintf("%s#%d", t.ID, i)})
	}
	p.tasks[t.ID] = &types.TaskSnapshot{ID: t.ID, Status: types.TaskDone, Type: t.Type}
	return nil
}

func (p *racePoster) PeekTask(_ context.Context, id string) (*types.TaskSnapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.tasks[id]; ok {
		cp := *s
		return &cp, nil
	}
	return nil, nil
}

func (p *racePoster) Subscribe(context.Context) (<-chan types.BlackboardEvent, error) {
	return p.bbCh, nil
}

func (p *racePoster) SubscribeTaskEvents(id string) (<-chan types.AgentStreamEvent, func()) {
	ch := make(chan types.AgentStreamEvent, 64)
	p.mu.Lock()
	p.subs[id] = append(p.subs[id], ch)
	p.activeSubs++
	p.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			p.mu.Lock()
			p.activeSubs--
			p.mu.Unlock()
		})
	}
}

func (p *racePoster) PublishTaskEvent(id string, ev types.AgentStreamEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.publishLocked(id, ev)
}

func (p *racePoster) SendTaskControl(string, types.TaskControl) bool { return true }
func (p *racePoster) FailTask(context.Context, string, string, []byte) error {
	return nil
}

func (p *racePoster) active() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.activeSubs
}

// waitHandoffDone 等待 intent 中出现 TriggerAgentHandoffDone。
func waitHandoffDone(t *testing.T, a *Agent) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case tr := <-a.intent:
			if tr == types.TriggerAgentHandoffDone {
				return
			}
		case <-timeout:
			t.Fatal("等待 TriggerAgentHandoffDone 超时")
		}
	}
}

// collectContents 取走流订阅 channel 中当前已到达的全部事件正文。
func collectContents(ch <-chan types.AgentStreamEvent) []string {
	var out []string
	for {
		select {
		case ev := <-ch:
			out = append(out, ev.Content)
		default:
			return out
		}
	}
}

func assertChildOrder(t *testing.T, got []string, childIDs []string, n int) {
	t.Helper()
	for _, cid := range childIDs {
		var seq []string
		for _, c := range got {
			if strings.HasPrefix(c, cid+"#") {
				seq = append(seq, c)
			}
		}
		if len(seq) != n {
			t.Fatalf("子任务 %s 收到 %d 条事件, want %d (all=%v)", cid, len(seq), n, got)
		}
		for i, c := range seq {
			if want := fmt.Sprintf("%s#%d", cid, i); c != want {
				t.Fatalf("子任务 %s 事件乱序: idx=%d got=%q want=%q", cid, i, c, want)
			}
		}
	}
}

func TestSpawnAgents_PreSubscribe_NoEventLoss(t *testing.T) {
	a := newTestHandoffAgent(t)
	p := newRacePoster(5)
	a.InjectHandoffPoster(p)
	stream := a.SubscribeStream(context.Background())

	ids, err := a.SpawnAgents(context.Background(), types.SpawnAgentsArgs{
		Tasks: []types.SpawnTaskSpec{{Prompt: "a"}, {Prompt: "b"}},
		Wait:  "all",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitHandoffDone(t, a)
	// 唤醒之前事件必须已全部转发（有序收尾）。
	assertChildOrder(t, collectContents(stream), ids, 5)
}

func TestTransferToAgent_PreSubscribe_NoEventLoss(t *testing.T) {
	a := newTestHandoffAgent(t)
	p := newRacePoster(5)
	a.InjectHandoffPoster(p)
	stream := a.SubscribeStream(context.Background())

	if _, err := a.executeTransferToAgent(context.Background(), "librarian", "x", types.TaintLow); err != nil {
		t.Fatal(err)
	}
	waitHandoffDone(t, a)
	p.mu.Lock()
	ids := append([]string(nil), p.postedOrder...)
	p.mu.Unlock()
	assertChildOrder(t, collectContents(stream), ids, 5)
}

// 子任务运行中持续产出事件，终态事件紧随其后：唤醒前事件不得丢失。
func TestWatchSubagents_DrainBeforeWake(t *testing.T) {
	a := newTestHandoffAgent(t)
	p := newRacePoster(0)
	a.InjectHandoffPoster(p)
	stream := a.SubscribeStream(context.Background())

	const id = "child-x"
	p.mu.Lock()
	p.tasks[id] = &types.TaskSnapshot{ID: id, Status: types.TaskExecuting}
	p.mu.Unlock()
	a.watchSubagentsCompletion([]string{id})
	time.Sleep(20 * time.Millisecond) // 等 watcher 订阅就绪

	for i := 0; i < 20; i++ {
		p.PublishTaskEvent(id, types.AgentStreamEvent{Type: types.AgentStreamEventStatus, Content: fmt.Sprintf("%s#%d", id, i)})
	}
	p.mu.Lock()
	p.tasks[id].Status = types.TaskDone
	p.mu.Unlock()
	p.bbCh <- types.BlackboardEvent{TaskID: id, Type: "task_completed"}

	waitHandoffDone(t, a)
	assertChildOrder(t, collectContents(stream), []string{id}, 20)
}

// 单子任务 watchHandoffCompletion：终态唤醒前同样排空。
func TestWatchHandoff_DrainBeforeWake(t *testing.T) {
	a := newTestHandoffAgent(t)
	p := newRacePoster(0)
	a.InjectHandoffPoster(p)
	stream := a.SubscribeStream(context.Background())

	const id = "child-y"
	p.mu.Lock()
	p.tasks[id] = &types.TaskSnapshot{ID: id, Status: types.TaskExecuting}
	p.mu.Unlock()
	a.watchHandoffCompletion(id)
	time.Sleep(20 * time.Millisecond)

	for i := 0; i < 20; i++ {
		p.PublishTaskEvent(id, types.AgentStreamEvent{Type: types.AgentStreamEventStatus, Content: fmt.Sprintf("%s#%d", id, i)})
	}
	p.mu.Lock()
	p.tasks[id].Status = types.TaskDone
	p.mu.Unlock()
	p.bbCh <- types.BlackboardEvent{TaskID: id, Type: "task_completed"}

	waitHandoffDone(t, a)
	assertChildOrder(t, collectContents(stream), []string{id}, 20)
}

func TestSpawnAgents_WaitNone_NoPreSubscribeLeak(t *testing.T) {
	a := newTestHandoffAgent(t)
	p := newRacePoster(1)
	a.InjectHandoffPoster(p)
	if _, err := a.SpawnAgents(context.Background(), types.SpawnAgentsArgs{
		Tasks: []types.SpawnTaskSpec{{Prompt: "a"}}, Wait: "none",
	}); err != nil {
		t.Fatal(err)
	}
	if n := p.active(); n != 0 {
		t.Fatalf("wait=none 不应产生订阅, active=%d", n)
	}
}

func TestSpawnAgents_PostFailure_ReleasesPreSubscribe(t *testing.T) {
	a := newTestHandoffAgent(t)
	p := newRacePoster(1)
	p.postErr = fmt.Errorf("boom")
	a.InjectHandoffPoster(p)
	if _, err := a.SpawnAgents(context.Background(), types.SpawnAgentsArgs{
		Tasks: []types.SpawnTaskSpec{{Prompt: "a"}}, Wait: "all",
	}); err == nil {
		t.Fatal("期望 PostTask 失败")
	}
	if n := p.active(); n != 0 {
		t.Fatalf("PostTask 失败后预订阅应注销, active=%d", n)
	}
}

func TestPreSubscribe_ReleasedOnClose(t *testing.T) {
	a := newTestHandoffAgent(t)
	p := newRacePoster(0)
	a.InjectHandoffPoster(p)
	a.preSubscribeChild("c1")
	a.preSubscribeChild("c2")
	if p.active() != 2 {
		t.Fatalf("active=%d want 2", p.active())
	}
	a.Close()
	if p.active() != 0 {
		t.Fatalf("Close 后应注销全部预订阅, active=%d", p.active())
	}
}
