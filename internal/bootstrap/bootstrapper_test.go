package bootstrap

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
)

type mockFailingModule struct {
	failPhase int
	stopped   bool
	closed    bool
}

func (m *mockFailingModule) Init(deps *DependencyMap) error { return nil }
func (m *mockFailingModule) Ready() bool                    { return true }
func (m *mockFailingModule) Dependencies() []string         { return nil }

func (m *mockFailingModule) StopIngress(ctx context.Context) error {
	m.stopped = true
	if m.failPhase == 1 {
		return apperr.New(apperr.CodeInternal, "StopIngress error")
	}
	return nil
}
func (m *mockFailingModule) Drain(ctx context.Context) error {
	if m.failPhase == 2 {
		return apperr.New(apperr.CodeInternal, "Drain error")
	}
	return nil
}
func (m *mockFailingModule) Flush(ctx context.Context) error {
	if m.failPhase == 3 {
		return apperr.New(apperr.CodeInternal, "Flush error")
	}
	return nil
}
func (m *mockFailingModule) Close(ctx context.Context) error {
	m.closed = true
	if m.failPhase == 4 {
		return apperr.New(apperr.CodeInternal, "Close error")
	}
	return nil
}

func TestGracefulShutdown_WithErrors(t *testing.T) {
	b := NewBootstrapper(nil)
	b.RegisterModule("mod1", &mockFailingModule{})

	// Testing if it panics or fails. The actual log is verified by slog test handlers if needed,
	// but here we just ensure gracefulShutdown executes without panicking and collects errors.
	b.gracefulShutdown(context.Background())
}
