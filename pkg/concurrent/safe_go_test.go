package concurrent

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type panicLogCapture struct {
	mu      sync.Mutex
	entries []map[string]any
	ctx     context.Context
	ready   chan struct{}
}

func (h *panicLogCapture) Enabled(context.Context, slog.Level) bool { return true }
func (h *panicLogCapture) Handle(ctx context.Context, record slog.Record) error {
	entry := map[string]any{"message": record.Message}
	record.Attrs(func(attr slog.Attr) bool {
		entry[attr.Key] = attr.Value.Any()
		return true
	})
	h.mu.Lock()
	h.entries = append(h.entries, entry)
	h.ctx = ctx
	h.mu.Unlock()
	select {
	case h.ready <- struct{}{}:
	default:
	}
	return nil
}
func (h *panicLogCapture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *panicLogCapture) WithGroup(string) slog.Handler      { return h }

type markedStringer struct{ value string }

func (m markedStringer) String() string { return m.value }

func TestSafeGoLogsPanicTypeWithoutPanicValue(t *testing.T) {
	previousHook := onPanic.Load()
	previousLogger := slog.Default()
	previousCount := PanicTotal.Load()
	t.Cleanup(func() {
		onPanic.Store(previousHook)
		slog.SetDefault(previousLogger)
		PanicTotal.Store(previousCount)
	})

	cases := []struct {
		name      string
		panicWith any
		typeName  string
	}{
		{name: "string", panicWith: "secret-string-marker", typeName: "string"},
		{name: "error", panicWith: errors.New("secret-error-marker"), typeName: "*errors.errorString"},
		{name: "stringer", panicWith: markedStringer{"secret-stringer-marker"}, typeName: "concurrent.markedStringer"},
	}
	for index, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			capture := &panicLogCapture{ready: make(chan struct{}, 1)}
			slog.SetDefault(slog.New(capture))
			hookCalls := 0
			SetOnPanic(func() { hookCalls++ })
			ctx := context.WithValue(context.Background(), panicTraceKey{}, "trace-for-test")
			SafeGo(ctx, "safe-go-test", func(context.Context) { panic(testCase.panicWith) })
			select {
			case <-capture.ready:
			case <-time.After(time.Second):
				t.Fatal("panic log was not emitted")
			}

			capture.mu.Lock()
			entry := capture.entries[len(capture.entries)-1]
			loggedCtx := capture.ctx
			capture.mu.Unlock()
			if entry["panic_type"] != testCase.typeName {
				t.Fatalf("panic type = %v, want %q", entry["panic_type"], testCase.typeName)
			}
			if entry["name"] != "safe-go-test" || !strings.Contains(entry["stack"].(string), "SafeGo") {
				t.Fatalf("missing goroutine name or stack: %#v", entry)
			}
			serialized := entry["message"].(string) + entry["stack"].(string)
			for _, marker := range []string{"secret-string-marker", "secret-error-marker", "secret-stringer-marker"} {
				if strings.Contains(serialized, marker) {
					t.Fatalf("panic payload leaked into logs: %q", marker)
				}
			}
			if loggedCtx.Value(panicTraceKey{}) != "trace-for-test" {
				t.Fatal("ErrorContext did not preserve the caller context")
			}
			if hookCalls != 1 || PanicTotal.Load() != previousCount+int64(index+1) {
				t.Fatalf("hook/count mismatch: hooks=%d count=%d", hookCalls, PanicTotal.Load()-previousCount)
			}
		})
	}
}

type panicTraceKey struct{}

func TestSafeGoNormalPathDoesNotCountOrLog(t *testing.T) {
	previousHook := onPanic.Load()
	previousLogger := slog.Default()
	previousCount := PanicTotal.Load()
	t.Cleanup(func() {
		onPanic.Store(previousHook)
		slog.SetDefault(previousLogger)
		PanicTotal.Store(previousCount)
	})
	capture := &panicLogCapture{ready: make(chan struct{}, 1)}
	slog.SetDefault(slog.New(capture))
	hookCalls := 0
	SetOnPanic(func() { hookCalls++ })
	done := make(chan struct{})
	SafeGo(context.Background(), "safe-go-normal", func(context.Context) { close(done) })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("normal SafeGo function did not complete")
	}
	if hookCalls != 0 || PanicTotal.Load() != previousCount || len(capture.entries) != 0 {
		t.Fatalf("normal path changed panic state: hooks=%d count=%d entries=%d", hookCalls, PanicTotal.Load()-previousCount, len(capture.entries))
	}
}

func TestSafeGoObserverPanicDoesNotEscapeRecoveryBoundary(t *testing.T) {
	previousHook := onPanic.Load()
	previousLogger := slog.Default()
	previousCount := PanicTotal.Load()
	t.Cleanup(func() {
		onPanic.Store(previousHook)
		slog.SetDefault(previousLogger)
		PanicTotal.Store(previousCount)
	})
	capture := &panicLogCapture{ready: make(chan struct{}, 2)}
	slog.SetDefault(slog.New(capture))
	SetOnPanic(func() { panic("secret-observer-marker") })
	SafeGo(context.Background(), "safe-go-observer-panic", func(context.Context) { panic("secret-original-marker") })
	for range 2 {
		select {
		case <-capture.ready:
		case <-time.After(time.Second):
			t.Fatal("observer and original panic logs were not both emitted")
		}
	}
	capture.mu.Lock()
	entries := append([]map[string]any(nil), capture.entries...)
	capture.mu.Unlock()
	if len(entries) != 2 {
		t.Fatalf("log records = %d, want observer error and recovered panic", len(entries))
	}
	combined := ""
	for _, entry := range entries {
		combined += entry["message"].(string) + " "
		for key, value := range entry {
			if key == "stack" {
				combined += value.(string)
			}
		}
	}
	if strings.Contains(combined, "secret-observer-marker") || strings.Contains(combined, "secret-original-marker") {
		t.Fatal("panic payload appeared in observer or original panic logs")
	}
	if entries[0]["callback_panic_type"] != "string" || entries[1]["panic_type"] != "string" {
		t.Fatalf("panic types missing from logs: %#v", entries)
	}
	if PanicTotal.Load() != previousCount+1 {
		t.Fatalf("recovered panic count = %d, want 1", PanicTotal.Load()-previousCount)
	}
}

func TestSetOnPanicNilClearsObserver(t *testing.T) {
	previousHook := onPanic.Load()
	t.Cleanup(func() { onPanic.Store(previousHook) })
	SetOnPanic(func() {})
	SetOnPanic(nil)
	if onPanic.Load() != nil {
		t.Fatal("SetOnPanic(nil) must clear the registered observer")
	}
}
