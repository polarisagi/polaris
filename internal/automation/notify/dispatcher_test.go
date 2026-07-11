package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/internal/store"
)

// mockPrefReader 是 PreferenceReader 的内存实现，供测试注入固定偏好值。
type mockPrefReader struct {
	mu     sync.Mutex
	values map[string]string
}

func newMockPrefReader(values map[string]string) *mockPrefReader {
	return &mockPrefReader{values: values}
}

func (m *mockPrefReader) GetPreference(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.values[key], nil
}

func TestDispatcher_Handle_DeliversToWebhook(t *testing.T) {
	var received NotificationEvent
	done := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode webhook body failed: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	prefs := newMockPrefReader(map[string]string{
		PrefWebhookURL: srv.URL,
	})
	d := NewDispatcher(prefs)

	ev := NotificationEvent{
		TaskID:   "task-1",
		TaskType: "cron",
		Pool:     "background",
		Success:  true,
	}
	payload, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event failed: %v", err)
	}

	if err := d.Handle(context.Background(), &store.OutboxRecord{Payload: payload}); err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}

	<-done
	if received.TaskID != "task-1" || !received.Success {
		t.Errorf("webhook received unexpected payload: %+v", received)
	}
}

func TestDispatcher_Handle_NoWebhookConfigured_Skips(t *testing.T) {
	d := NewDispatcher(newMockPrefReader(nil))
	ev := NotificationEvent{TaskID: "task-2"}
	payload, _ := json.Marshal(ev)

	if err := d.Handle(context.Background(), &store.OutboxRecord{Payload: payload}); err != nil {
		t.Fatalf("expected nil error when webhook not configured, got: %v", err)
	}
}

func TestDispatcher_Handle_Disabled_Skips(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	prefs := newMockPrefReader(map[string]string{
		PrefWebhookURL: srv.URL,
		PrefEnabled:    "false",
	})
	d := NewDispatcher(prefs)
	ev := NotificationEvent{TaskID: "task-3"}
	payload, _ := json.Marshal(ev)

	if err := d.Handle(context.Background(), &store.OutboxRecord{Payload: payload}); err != nil {
		t.Fatalf("expected nil error when notifications disabled, got: %v", err)
	}
	if called {
		t.Error("webhook should not have been called when notifications disabled")
	}
}

func TestDispatcher_Handle_WebhookErrorStatus_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	prefs := newMockPrefReader(map[string]string{PrefWebhookURL: srv.URL})
	d := NewDispatcher(prefs)
	ev := NotificationEvent{TaskID: "task-4"}
	payload, _ := json.Marshal(ev)

	if err := d.Handle(context.Background(), &store.OutboxRecord{Payload: payload}); err == nil {
		t.Fatal("expected error on non-2xx webhook response (to trigger OutboxWorker retry)")
	}
}

func TestDispatcher_Handle_MalformedPayload_SkipsWithoutError(t *testing.T) {
	d := NewDispatcher(newMockPrefReader(nil))
	if err := d.Handle(context.Background(), &store.OutboxRecord{Payload: []byte("not json")}); err != nil {
		t.Fatalf("expected nil error for malformed payload (skip, not retry), got: %v", err)
	}
}
