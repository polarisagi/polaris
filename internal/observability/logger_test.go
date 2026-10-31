package observability

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

type failingLogHandler struct {
	calls int
	err   error
}

func (h *failingLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *failingLogHandler) Handle(context.Context, slog.Record) error {
	h.calls++
	return h.err
}
func (h *failingLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *failingLogHandler) WithGroup(string) slog.Handler      { return h }

type recordingLogHandler struct {
	calls int
}

func (h *recordingLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingLogHandler) Handle(context.Context, slog.Record) error {
	h.calls++
	return nil
}
func (h *recordingLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingLogHandler) WithGroup(string) slog.Handler      { return h }

func TestTeeHandlerAttemptsEverySinkAndFallsBack(t *testing.T) {
	first := &failingLogHandler{err: errors.New("disk full")}
	second := &recordingLogHandler{}
	var fallback bytes.Buffer
	h := &TeeHandler{
		handlers: []slog.Handler{first, second},
		fallback: slog.NewTextHandler(&fallback, nil),
	}

	slog.New(h).Error("write failed", "err", errors.New("disk full"))

	if first.calls != 1 || second.calls != 1 {
		t.Fatalf("all sinks must be attempted: first=%d second=%d", first.calls, second.calls)
	}
	if !bytes.Contains(fallback.Bytes(), []byte("write failed")) {
		t.Fatalf("failed sink record missing from fallback: %q", fallback.String())
	}
}

func TestTeeHandlerPreservesAttrsAndGroupsOnDebugFallback(t *testing.T) {
	failed := &failingLogHandler{err: errors.New("disk full")}
	var fallback bytes.Buffer
	h := &TeeHandler{
		handlers: []slog.Handler{failed},
		fallback: slog.NewTextHandler(&fallback, &slog.HandlerOptions{Level: slog.LevelDebug}),
	}

	slog.New(h).WithGroup("request").With("request_id", "req-123").Debug("debug failure")

	for _, want := range []string{"debug failure", "request", "request_id", "req-123"} {
		if !bytes.Contains(fallback.Bytes(), []byte(want)) {
			t.Errorf("stderr fallback lost %q: %s", want, fallback.String())
		}
	}
}

func TestTeeHandlerEnabledComesOnlyFromSinks(t *testing.T) {
	h := NewTeeHandler()
	if h.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("stderr fallback must not make an empty tee appear enabled")
	}
}

func TestSetupLoggerSecuresExistingLogs(t *testing.T) {
	old := slog.Default()
	defer slog.SetDefault(old)
	t.Setenv("LOG_LEVEL", "debug")

	dir := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(dir, "polaris.log")
	if err := os.WriteFile(mainPath, []byte("old log"), 0o644); err != nil {
		t.Fatal(err)
	}

	closer := SetupLogger(dir)
	slog.Warn("permissions check", "err", errors.New("test"))
	if err := closer.Close(); err != nil {
		t.Fatalf("close logger: %v", err)
	}

	for _, path := range []string{dir, mainPath, filepath.Join(dir, "polaris.error.log")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		want := os.FileMode(0o600)
		if path == dir {
			want = 0o700
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s permissions: want %04o, got %04o", path, want, got)
		}
	}
}

func TestSetupLoggerRejectsSymlinkDirectoryWithoutChangingTarget(t *testing.T) {
	old := slog.Default()
	defer slog.SetDefault(old)

	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "logs")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	closer := SetupLogger(link)
	defer closer.Close()
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("symlink target permissions changed: want 0755, got %04o", got)
	}
}
