package chat

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/gateway/session"
)

// fakeOrch 模拟回合：发 session 事件与一个 token，然后阻塞到 release 或 ctx 取消。
type fakeOrch struct {
	release   chan struct{}
	completed atomic.Int32
	cancelled atomic.Int32
}

func (f *fakeOrch) RunTurn(ctx context.Context, req session.Request, sink session.Sink) (*session.Result, error) {
	sid := req.SessionID
	if sid == "" {
		sid = session.NewSessionID()
	}
	_ = sink.Emit(session.Event{Kind: session.KindSession, Payload: map[string]any{"session_id": sid}})
	_ = sink.Emit(session.Event{Kind: session.KindDelta, Text: "hello"})
	select {
	case <-f.release:
		f.completed.Add(1)
		_ = sink.Emit(session.Event{Kind: session.KindComplete, Payload: map[string]any{"session_id": sid}})
		return &session.Result{SessionID: sid}, nil
	case <-ctx.Done():
		f.cancelled.Add(1)
		return &session.Result{SessionID: sid, Aborted: true}, nil
	}
}

func newTurnServer(t *testing.T) (*httptest.Server, *fakeOrch, *ChatHandler) {
	t.Helper()
	f := &fakeOrch{release: make(chan struct{})}
	h := &ChatHandler{Orchestrator: f}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/agent/stream", h.HandleAgentStream)
	mux.HandleFunc("GET /v1/sessions/{sessionID}/stream", h.HandleSessionStream)
	mux.HandleFunc("POST /v1/sessions/{sessionID}/abort", h.HandleAbortSession)
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		srv.Close()
		h.Shutdown(context.Background())
	})
	return srv, f, h
}

type sseEv struct{ id, name, data string }

func readEvents(t *testing.T, body io.Reader, stopAt string) []sseEv {
	t.Helper()
	var out []sseEv
	cur := sseEv{}
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			cur.id = line[4:]
		case strings.HasPrefix(line, "event: "):
			cur.name = line[7:]
		case strings.HasPrefix(line, "data: "):
			cur.data = line[6:]
		case line == "" && cur.name != "":
			out = append(out, cur)
			if cur.name == stopAt {
				return out
			}
			cur = sseEv{}
		}
	}
	return out
}

func postStream(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url+"/v1/agent/stream", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func firstSessionID(t *testing.T, resp *http.Response) string {
	t.Helper()
	evs := readEvents(t, resp.Body, "session")
	if len(evs) == 0 || evs[0].name != "session" || evs[0].id == "" {
		t.Fatalf("no session event: %+v", evs)
	}
	var p struct {
		SessionID string `json:"session_id"`
		TurnID    string `json:"turn_id"`
	}
	if err := json.Unmarshal([]byte(evs[0].data), &p); err != nil || p.SessionID == "" || p.TurnID == "" {
		t.Fatalf("bad session payload %q: %v", evs[0].data, err)
	}
	return p.SessionID
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timeout: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAgentStream_DisconnectKeepsTurnRunningAndReplays(t *testing.T) {
	srv, f, h := newTurnServer(t)
	resp := postStream(t, srv.URL, `{"input":"hi"}`)
	sid := firstSessionID(t, resp)
	resp.Body.Close() // 断开连接

	time.Sleep(100 * time.Millisecond)
	if f.cancelled.Load() != 0 || !h.turns().Running(sid) {
		t.Fatal("disconnect must not cancel the turn")
	}

	// 运行中重订阅：回放 + 接实时
	re, err := http.Get(srv.URL + "/v1/sessions/" + sid + "/stream?after=0")
	if err != nil {
		t.Fatal(err)
	}
	defer re.Body.Close()
	go func() { time.Sleep(50 * time.Millisecond); close(f.release) }()
	evs := readEvents(t, re.Body, "complete")
	var names []string
	for _, e := range evs {
		names = append(names, e.name)
	}
	if strings.Join(names, ",") != "session,token,complete" {
		t.Fatalf("replay+live events: %v", names)
	}
	eventually(t, "turn completed", func() bool { return f.completed.Load() == 1 && !h.turns().Running(sid) })

	// 结束后保留期内 after=1 只回放尾部
	re2, err := http.Get(srv.URL + "/v1/sessions/" + sid + "/stream?after=1")
	if err != nil {
		t.Fatal(err)
	}
	defer re2.Body.Close()
	evs = readEvents(t, re2.Body, "complete")
	if len(evs) != 2 || evs[0].name != "token" {
		t.Fatalf("after=1 replay: %+v", evs)
	}
}

func TestAgentStream_SameSessionBusy409(t *testing.T) {
	srv, f, _ := newTurnServer(t)
	resp := postStream(t, srv.URL, `{"input":"hi"}`)
	sid := firstSessionID(t, resp)
	defer resp.Body.Close()
	r2 := postStream(t, srv.URL, `{"input":"again","session_id":"`+sid+`"}`)
	defer r2.Body.Close()
	b, _ := io.ReadAll(r2.Body)
	if r2.StatusCode != http.StatusConflict || !strings.Contains(string(b), "session_busy") {
		t.Fatalf("want 409 session_busy, got %d %s", r2.StatusCode, b)
	}
	close(f.release)
}

func TestAgentStream_DifferentSessionsParallel(t *testing.T) {
	srv, f, _ := newTurnServer(t)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := postStream(t, srv.URL, `{"input":"hi"}`)
			defer resp.Body.Close()
			evs := readEvents(t, resp.Body, "complete")
			if len(evs) == 0 || evs[len(evs)-1].name != "complete" {
				t.Errorf("no complete: %+v", evs)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(f.release)
	wg.Wait()
	if f.completed.Load() != 2 {
		t.Fatalf("completed=%d", f.completed.Load())
	}
}

func TestSessionAbort(t *testing.T) {
	srv, f, h := newTurnServer(t)
	resp := postStream(t, srv.URL, `{"input":"hi"}`)
	sid := firstSessionID(t, resp)
	resp.Body.Close()

	ab, err := http.Post(srv.URL+"/v1/sessions/"+sid+"/abort", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(ab.Body)
	ab.Body.Close()
	if !strings.Contains(string(b), `"aborted":true`) {
		t.Fatalf("abort resp %s", b)
	}
	eventually(t, "cancelled", func() bool { return f.cancelled.Load() == 1 && !h.turns().Running(sid) })
}

func TestSessionStream_NoTurn204(t *testing.T) {
	srv, _, _ := newTurnServer(t)
	resp, err := http.Get(srv.URL + "/v1/sessions/sess_none/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204 got %d", resp.StatusCode)
	}
}
