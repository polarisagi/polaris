package session

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func drain(sub *Subscription) []SeqEvent {
	var out []SeqEvent
	for {
		ev, ok, _ := sub.Poll()
		if !ok {
			return out
		}
		out = append(out, ev)
	}
}

func newTestTurn(t *testing.T, cfg TurnHubConfig, sid string) (*TurnHub, *Turn, *int) {
	t.Helper()
	h := NewTurnHub(cfg)
	cancelled := 0
	turn, res := h.Begin(sid, func() { cancelled++ })
	if res != BeginOK {
		t.Fatalf("begin: %v", res)
	}
	return h, turn, &cancelled
}

func TestTurnHub_ReplayAfterSeq(t *testing.T) {
	_, turn, _ := newTestTurn(t, TurnHubConfig{}, "s1")
	for i := 0; i < 5; i++ {
		_ = turn.Emit(Event{Kind: KindStatus, Payload: map[string]any{"n": i}})
	}
	all := drain(turn.Subscribe(0))
	if len(all) != 5 || all[0].Seq != 1 {
		t.Fatalf("replay from 0: %+v", all)
	}
	tail := drain(turn.Subscribe(3))
	if len(tail) != 2 || tail[0].Seq != 4 {
		t.Fatalf("replay after 3: %+v", tail)
	}
}

func TestTurnHub_LiveWakeAndUnsubscribe(t *testing.T) {
	_, turn, _ := newTestTurn(t, TurnHubConfig{}, "s1")
	sub := turn.Subscribe(0)
	_ = turn.Emit(Event{Kind: KindDelta, Text: "a"})
	select {
	case <-sub.Wake():
	case <-time.After(time.Second):
		t.Fatal("no wake")
	}
	sub.Close()
	_ = turn.Emit(Event{Kind: KindDelta, Text: "b"}) // 退订后写入不得阻塞或 panic
	turn.Finish()
	if !turn.Done() {
		t.Fatal("not done")
	}
}

func TestTurnHub_FinishAppendsTerminal(t *testing.T) {
	_, turn, _ := newTestTurn(t, TurnHubConfig{}, "s1")
	_ = turn.Emit(Event{Kind: KindDelta, Text: "x"})
	turn.Finish()
	turn.Finish() // 幂等
	evs := drain(turn.Subscribe(0))
	last := evs[len(evs)-1]
	if last.Event.Kind != KindComplete || last.Event.Payload["aborted"] != true {
		t.Fatalf("expected synthetic complete, got %+v", last)
	}
	if len(evs) != 2 {
		t.Fatalf("finish not idempotent: %d events", len(evs))
	}
}

func TestTurnHub_BoundedMergeKeepsText(t *testing.T) {
	_, turn, _ := newTestTurn(t, TurnHubConfig{MaxEvents: 100}, "s1")
	var want strings.Builder
	for i := 0; i < 1000; i++ {
		s := fmt.Sprintf("%d,", i)
		want.WriteString(s)
		_ = turn.Emit(Event{Kind: KindDelta, Text: s})
	}
	turn.mu.Lock()
	n := len(turn.events)
	turn.mu.Unlock()
	if n > 100 {
		t.Fatalf("buffer unbounded: %d", n)
	}
	var got strings.Builder
	for _, e := range drain(turn.Subscribe(0)) {
		got.WriteString(e.Event.Text)
	}
	if got.String() != want.String() {
		t.Fatal("merged text differs from original")
	}
}

func TestTurnHub_MergedEventNotDuplicatedForPartialReader(t *testing.T) {
	_, turn, _ := newTestTurn(t, TurnHubConfig{MaxEvents: 100}, "s1")
	sub := turn.Subscribe(0)
	for i := 0; i < 5; i++ {
		_ = turn.Emit(Event{Kind: KindDelta, Text: "x"})
	}
	if got := len(drain(sub)); got != 5 {
		t.Fatalf("pre-read %d", got)
	}
	for i := 0; i < 500; i++ {
		_ = turn.Emit(Event{Kind: KindDelta, Text: "y"})
	}
	var got strings.Builder
	for _, e := range drain(sub) {
		got.WriteString(e.Event.Text)
	}
	if strings.Contains(got.String(), "x") {
		t.Fatal("already-read text redelivered")
	}
}

func TestTurnHub_BytesBound(t *testing.T) {
	_, turn, _ := newTestTurn(t, TurnHubConfig{MaxBytes: 4096}, "s1")
	for i := 0; i < 200; i++ {
		_ = turn.Emit(Event{Kind: KindStatus, Payload: map[string]any{"message": strings.Repeat("z", 200)}})
	}
	turn.mu.Lock()
	b := turn.bytes
	turn.mu.Unlock()
	if b > 4096 {
		t.Fatalf("bytes unbounded: %d", b)
	}
}

func TestTurnHub_BusyAndRetentionExpiry(t *testing.T) {
	h := NewTurnHub(TurnHubConfig{Retention: 30 * time.Millisecond})
	turn, _ := h.Begin("s1", func() {})
	if _, res := h.Begin("s1", func() {}); res != BeginBusy {
		t.Fatalf("want busy, got %v", res)
	}
	if !h.Running("s1") || !h.RunningSet()["s1"] {
		t.Fatal("should be running")
	}
	turn.Finish()
	if h.Running("s1") || h.Get("s1") == nil {
		t.Fatal("finished turn should be retained but not running")
	}
	deadline := time.Now().Add(2 * time.Second)
	for h.Get("s1") != nil {
		if time.Now().After(deadline) {
			t.Fatal("not expired")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 结束后同会话可开新回合
	if _, res := h.Begin("s1", func() {}); res != BeginOK {
		t.Fatal("begin after expiry")
	}
}

func TestTurnHub_NewSessionBind(t *testing.T) {
	h := NewTurnHub(TurnHubConfig{})
	turn, _ := h.Begin("", func() {})
	_ = turn.Emit(Event{Kind: KindSession, Payload: map[string]any{"session_id": "sess_abc"}})
	if !h.Running("sess_abc") || h.Get("sess_abc") != turn {
		t.Fatal("not bound")
	}
	ev := drain(turn.Subscribe(0))[0]
	if ev.Event.Payload["turn_id"] != turn.ID {
		t.Fatalf("turn_id missing: %+v", ev.Event.Payload)
	}
}

func TestTurnHub_AbortAndShutdown(t *testing.T) {
	h := NewTurnHub(TurnHubConfig{})
	var cancelled int
	turn, _ := h.Begin("s1", func() { cancelled++ })
	if !h.Abort("s1") || cancelled != 1 {
		t.Fatal("abort should cancel")
	}
	turn.Finish()
	if h.Abort("s1") {
		t.Fatal("abort on finished turn")
	}

	t2, _ := h.Begin("s2", func() {})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { time.Sleep(20 * time.Millisecond); t2.Finish() }()
	h.Shutdown(ctx)
	if _, res := h.Begin("s3", func() {}); res != BeginClosed {
		t.Fatal("begin after shutdown")
	}
}

func TestTurnHub_ConcurrentRace(t *testing.T) {
	_, turn, _ := newTestTurn(t, TurnHubConfig{MaxEvents: 64}, "s1")
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub := turn.Subscribe(0)
			defer sub.Close()
			for {
				if _, ok, done := sub.Poll(); ok {
					continue
				} else if done {
					return
				}
				<-sub.Wake()
			}
		}()
	}
	for i := 0; i < 2000; i++ {
		_ = turn.Emit(Event{Kind: KindDelta, Text: "t"})
	}
	turn.Finish()
	wg.Wait()
}
