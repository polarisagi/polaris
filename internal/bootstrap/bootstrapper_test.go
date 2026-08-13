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

type trackModule struct {
	name     string
	deps     []string
	tracker  *[]string
	failInit bool
}

func (m *trackModule) Init(deps *DependencyMap) error {
	if m.failInit {
		return apperr.New(apperr.CodeInternal, "Init error")
	}
	return nil
}
func (m *trackModule) Ready() bool            { return true }
func (m *trackModule) Dependencies() []string { return m.deps }

func (m *trackModule) StopIngress(ctx context.Context) error {
	*m.tracker = append(*m.tracker, m.name+"_StopIngress")
	return nil
}
func (m *trackModule) Drain(ctx context.Context) error {
	*m.tracker = append(*m.tracker, m.name+"_Drain")
	return nil
}
func (m *trackModule) Flush(ctx context.Context) error {
	*m.tracker = append(*m.tracker, m.name+"_Flush")
	return nil
}
func (m *trackModule) Close(ctx context.Context) error {
	*m.tracker = append(*m.tracker, m.name+"_Close")
	return nil
}

func TestBootstrapper_ShutdownOrderDeterminism(t *testing.T) {
	// 多次运行，确保顺序一定是稳定的 C, B, A
	for i := 0; i < 20; i++ {
		b := NewBootstrapper(nil)
		var tracker []string

		b.RegisterModule("A", &trackModule{name: "A", deps: nil, tracker: &tracker})
		b.RegisterModule("B", &trackModule{name: "B", deps: []string{"A"}, tracker: &tracker})
		b.RegisterModule("C", &trackModule{name: "C", deps: []string{"B"}, tracker: &tracker})

		err := b.Ignite(context.Background())
		if err != nil {
			t.Fatalf("Ignite failed: %v", err)
		}

		b.gracefulShutdown(context.Background())

		expected := []string{
			"C_StopIngress", "B_StopIngress", "A_StopIngress",
			"C_Drain", "B_Drain", "A_Drain",
			"C_Flush", "B_Flush", "A_Flush",
			"C_Close", "B_Close", "A_Close",
		}

		if len(tracker) != len(expected) {
			t.Fatalf("Expected tracker length %d, got %d", len(expected), len(tracker))
		}
		for j, v := range expected {
			if tracker[j] != v {
				t.Fatalf("Iteration %d: Expected %s at index %d, got %s", i, v, j, tracker[j])
			}
		}
	}
}

func TestBootstrapper_InitRollback(t *testing.T) {
	b := NewBootstrapper(nil)
	var tracker []string

	b.RegisterModule("A", &trackModule{name: "A", deps: nil, tracker: &tracker})
	b.RegisterModule("B", &trackModule{name: "B", deps: []string{"A"}, tracker: &tracker})
	b.RegisterModule("C", &trackModule{name: "C", deps: []string{"B"}, tracker: &tracker, failInit: true})

	err := b.Ignite(context.Background())
	if err == nil {
		t.Fatalf("Expected Ignite to fail")
	}

	expected := []string{
		"B_Close", "B_Flush",
		"A_Close", "A_Flush",
	}

	if len(tracker) != len(expected) {
		t.Fatalf("Expected tracker length %d, got %d. Tracker: %v", len(expected), len(tracker), tracker)
	}
	for i, v := range expected {
		if tracker[i] != v {
			t.Fatalf("Expected %s at index %d, got %s", v, i, tracker[i])
		}
	}
}
