package fsm

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type mockExtensionActivator struct {
	err error
}

func (m *mockExtensionActivator) FindAndActivate(ctx context.Context, goal string) ([]ExtActivatedHint, error) {
	if m.err != nil {
		return nil, m.err
	}
	return []ExtActivatedHint{{ToolName: "mock"}}, nil
}

func TestActivateExtWithRetry_Success(t *testing.T) {
	activator := &mockExtensionActivator{err: nil}
	sm := NewStateMachine(&dummyContextBuilder{})
	sm.activator = activator

	hints, degraded := sm.activateExtWithRetry(context.Background(), "goal")
	if degraded {
		t.Error("expected degraded to be false")
	}
	if len(hints) != 1 {
		t.Error("expected 1 hint")
	}
}

func TestActivateExtWithRetry_Degraded(t *testing.T) {
	activator := &mockExtensionActivator{err: apperr.New(apperr.CodeInternal, "timeout")}
	sm := NewStateMachine(&dummyContextBuilder{})
	sm.activator = activator

	before := metrics.GlobalReplanExtActivationDegradedTotal.Load()

	hints, degraded := sm.activateExtWithRetry(context.Background(), "goal")
	if !degraded {
		t.Error("expected degraded to be true")
	}
	if len(hints) != 0 {
		t.Error("expected 0 hints")
	}

	after := metrics.GlobalReplanExtActivationDegradedTotal.Load()
	if after != before+1 {
		t.Errorf("expected metric to increment by 1, before: %d, after: %d", before, after)
	}
}
