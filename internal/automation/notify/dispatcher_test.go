package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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

type mockRoundTripperFunc func(req *http.Request) *http.Response

func (f mockRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req), nil
}

func TestDispatcher_Handle_DeliversToWebhook(t *testing.T) {
	var received NotificationEvent

	prefs := newMockPrefReader(map[string]string{
		PrefWebhookURL: "http://dummy",
	})
	d := NewDispatcher(prefs)
	d.httpClient = &http.Client{
		Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
			if err := json.NewDecoder(req.Body).Decode(&received); err != nil {
				t.Errorf("decode webhook body failed: %v", err)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("")),
			}
		}),
	}

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
	prefs := newMockPrefReader(map[string]string{
		PrefWebhookURL: "http://dummy",
		PrefEnabled:    "false",
	})
	d := NewDispatcher(prefs)
	d.httpClient = &http.Client{
		Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
			called = true
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("")),
			}
		}),
	}
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
	prefs := newMockPrefReader(map[string]string{PrefWebhookURL: "http://dummy"})
	d := NewDispatcher(prefs)
	d.httpClient = &http.Client{
		Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Body:       io.NopCloser(strings.NewReader("")),
			}
		}),
	}
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
