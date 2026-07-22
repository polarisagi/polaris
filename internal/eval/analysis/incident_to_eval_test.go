package analysis_test

import (
	"github.com/polarisagi/polaris/internal/eval/analysis"
	"github.com/polarisagi/polaris/internal/eval/harness"

	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/eval/control"
	"github.com/polarisagi/polaris/internal/protocol"
)

type mockStore struct {
	protocol.Store
	data map[string][]byte
}

func (m *mockStore) Put(ctx context.Context, key []byte, value []byte) error {
	m.data[string(key)] = value
	return nil
}

func (m *mockStore) Get(ctx context.Context, key []byte) ([]byte, error) {
	val, ok := m.data[string(key)]
	if !ok {
		return nil, nil
	}
	return val, nil
}

func (m *mockStore) Delete(ctx context.Context, key []byte) error {
	delete(m.data, string(key))
	return nil
}

func (m *mockStore) Scan(ctx context.Context, prefix []byte) (protocol.Iterator, error) {
	return nil, nil // not implemented
}

type dummyPIIDetector struct{}

func (d *dummyPIIDetector) Scrub(text string) string {
	return strings.ReplaceAll(text, "secret", "***")
}

func TestIncidentToEvalConverter_Integration(t *testing.T) {
	ctx := context.Background()
	mStore := &mockStore{data: make(map[string][]byte)}
	sqliteStore := harness.NewSQLiteEvalStore(mStore, control.NewEngine(nil))
	converter := analysis.NewIncidentToEvalConverter(sqliteStore, &dummyPIIDetector{})

	payload := analysis.IncidentPayload{
		Input:           map[string]any{"q": "hello"},
		Expected:        map[string]any{"a": "world"},
		Actual:          map[string]any{"a": "fail"},
		Details:         "A secret fail",
		NeedsHumanAudit: false,
		TaintLevel:      1,
	}
	b, _ := json.Marshal(payload)

	c, err := converter.Convert(ctx, b)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c == nil {
		t.Fatalf("expected case, got nil")
	}

	if !strings.Contains(c.Description, "***") {
		t.Errorf("expected scrubbed details, got: %v", c.Description)
	}

	// Verify case is in pending_review
	oldKey := "eval:case:pending_review:incident:" + c.ID
	if _, ok := mStore.data[oldKey]; !ok {
		t.Errorf("expected case to be in pending_review partition")
	}

}
