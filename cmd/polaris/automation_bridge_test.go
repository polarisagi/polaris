package main

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type mockAutoSvc struct {
	listFn                 func(ctx context.Context) ([]protocol.AutomationSummary, error)
	savePromptFn           func(ctx context.Context, in protocol.PromptAutomationInput) (string, string, error)
	deleteFn               func(ctx context.Context, id string) error
	runNowFn               func(ctx context.Context, id string) (string, error)
	countEnabledBySourceFn func(ctx context.Context, source string) (int, error)
}

func (m *mockAutoSvc) List(ctx context.Context) ([]protocol.AutomationSummary, error) {
	if m.listFn != nil {
		return m.listFn(ctx)
	}
	return nil, nil
}

func (m *mockAutoSvc) SavePrompt(ctx context.Context, in protocol.PromptAutomationInput) (string, string, error) {
	if m.savePromptFn != nil {
		return m.savePromptFn(ctx, in)
	}
	return "", "", nil
}

func (m *mockAutoSvc) Delete(ctx context.Context, id string) error {
	if m.deleteFn != nil {
		return m.deleteFn(ctx, id)
	}
	return nil
}

func (m *mockAutoSvc) RunNow(ctx context.Context, id string) (string, error) {
	if m.runNowFn != nil {
		return m.runNowFn(ctx, id)
	}
	return "", nil
}

func (m *mockAutoSvc) CountEnabledBySource(ctx context.Context, source string) (int, error) {
	if m.countEnabledBySourceFn != nil {
		return m.countEnabledBySourceFn(ctx, source)
	}
	return 0, nil
}

func TestAutomationBridge_UnboundReportsError(t *testing.T) {
	b := &automationBridge{}
	ctx := context.Background()

	if _, err := b.List(ctx); !apperr.IsCode(err, apperr.CodeUnimplemented) {
		t.Fatalf("expected CodeUnimplemented for List when unbound, got %v", err)
	}
	if _, _, err := b.SavePrompt(ctx, protocol.PromptAutomationInput{}); !apperr.IsCode(err, apperr.CodeUnimplemented) {
		t.Fatalf("expected CodeUnimplemented for SavePrompt when unbound, got %v", err)
	}
	if err := b.Delete(ctx, "id1"); !apperr.IsCode(err, apperr.CodeUnimplemented) {
		t.Fatalf("expected CodeUnimplemented for Delete when unbound, got %v", err)
	}
	if _, err := b.RunNow(ctx, "id1"); !apperr.IsCode(err, apperr.CodeUnimplemented) {
		t.Fatalf("expected CodeUnimplemented for RunNow when unbound, got %v", err)
	}
	if _, err := b.CountEnabledBySource(ctx, "agent"); !apperr.IsCode(err, apperr.CodeUnimplemented) {
		t.Fatalf("expected CodeUnimplemented for CountEnabledBySource when unbound, got %v", err)
	}
}

func TestAutomationBridge_BoundDelegates(t *testing.T) {
	b := &automationBridge{}
	ctx := context.Background()

	mock := &mockAutoSvc{
		listFn: func(ctx context.Context) ([]protocol.AutomationSummary, error) {
			return []protocol.AutomationSummary{{ID: "auto-1", Name: "test"}}, nil
		},
		savePromptFn: func(ctx context.Context, in protocol.PromptAutomationInput) (string, string, error) {
			return "saved-id", "2026-10-15T00:00:00Z", nil
		},
		deleteFn: func(ctx context.Context, id string) error {
			return nil
		},
		runNowFn: func(ctx context.Context, id string) (string, error) {
			return "run-123", nil
		},
		countEnabledBySourceFn: func(ctx context.Context, source string) (int, error) {
			return 5, nil
		},
	}
	b.Bind(mock)

	list, err := b.List(ctx)
	if err != nil || len(list) != 1 || list[0].ID != "auto-1" {
		t.Fatalf("List delegation failed: got %v, err=%v", list, err)
	}
	id, nextRun, err := b.SavePrompt(ctx, protocol.PromptAutomationInput{Prompt: "p"})
	if err != nil || id != "saved-id" || nextRun != "2026-10-15T00:00:00Z" {
		t.Fatalf("SavePrompt delegation failed: id=%q nextRun=%q err=%v", id, nextRun, err)
	}
	if err := b.Delete(ctx, "auto-1"); err != nil {
		t.Fatalf("Delete delegation failed: %v", err)
	}
	runID, err := b.RunNow(ctx, "auto-1")
	if err != nil || runID != "run-123" {
		t.Fatalf("RunNow delegation failed: %v", err)
	}
	count, err := b.CountEnabledBySource(ctx, "agent")
	if err != nil || count != 5 {
		t.Fatalf("CountEnabledBySource delegation failed: %v", err)
	}
}
