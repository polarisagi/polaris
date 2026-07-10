package bootstrap

import (
	"context"
	"errors"
	"testing"
)

type mockFailingModule struct{}

func (m *mockFailingModule) Init(deps *DependencyMap) error { return nil }
func (m *mockFailingModule) Ready() bool                    { return true }
func (m *mockFailingModule) Dependencies() []string         { return nil }

func (m *mockFailingModule) StopIngress(ctx context.Context) error {
	return errors.New("StopIngress error")
}
func (m *mockFailingModule) Drain(ctx context.Context) error {
	return errors.New("Drain error")
}
func (m *mockFailingModule) Flush(ctx context.Context) error {
	return errors.New("Flush error")
}
func (m *mockFailingModule) Close(ctx context.Context) error {
	return errors.New("Close error")
}

func TestGracefulShutdown_WithErrors(t *testing.T) {
	b := NewBootstrapper(nil)
	b.RegisterModule("mod1", &mockFailingModule{})

	// Testing if it panics or fails. The actual log is verified by slog test handlers if needed,
	// but here we just ensure gracefulShutdown executes without panicking and collects errors.
	b.gracefulShutdown(context.Background())
}
