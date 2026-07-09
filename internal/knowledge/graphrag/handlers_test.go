package graphrag

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/store"
)

func TestGraphBuildOutboxHandler(t *testing.T) {
	handler := NewGraphBuildOutboxHandler(nil)

	// Test empty payload
	entry := &store.OutboxRecord{
		Payload: []byte(`{}`),
	}
	err := handler.Handle(context.Background(), entry)
	if err != nil {
		t.Errorf("expected no error for missing doc_id, got %v", err)
	}

	// Test invalid JSON
	entry.Payload = []byte(`{invalid}`)
	err = handler.Handle(context.Background(), entry)
	if err == nil {
		t.Errorf("expected error for invalid JSON")
	}
}

func TestSummaryGenOutboxHandler(t *testing.T) {
	handler := NewSummaryGenOutboxHandler(nil, nil)

	// Test empty payload
	entry := &store.OutboxRecord{
		Payload: []byte(`{}`),
	}
	err := handler.Handle(context.Background(), entry)
	if err != nil {
		t.Errorf("expected no error for missing doc_id, got %v", err)
	}

	// Test invalid JSON
	entry.Payload = []byte(`{invalid}`)
	err = handler.Handle(context.Background(), entry)
	if err == nil {
		t.Errorf("expected error for invalid JSON")
	}
}
