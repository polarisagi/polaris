package connector

import (
	"context"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/knowledge"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockConnector struct {
	events chan types.ChangeEvent
}

func (m *mockConnector) ID() string { return "mock" }
func (m *mockConnector) List(ctx context.Context) ([]*types.DocumentRef, error) {
	return []*types.DocumentRef{{URI: "doc1"}}, nil
}
func (m *mockConnector) Fetch(ctx context.Context, doc *types.DocumentRef) (*types.SyncDocument, error) {
	return &types.SyncDocument{URI: doc.URI, Content: []byte("content")}, nil
}
func (m *mockConnector) Watch(ctx context.Context) (<-chan types.ChangeEvent, error) {
	return m.events, nil
}

type mockPipeline struct {
	ingested int
}

func (m *mockPipeline) Ingest(ctx context.Context, doc *knowledge.Document, taintLevel int) (*knowledge.DocTree, error) {
	m.ingested++
	return &knowledge.DocTree{}, nil
}
func (m *mockPipeline) Delete(ctx context.Context, uri string) error { return nil }

func TestSyncScheduler_NewAndStart(t *testing.T) {
	events := make(chan types.ChangeEvent, 10)
	conn := &mockConnector{events: events}
	pipe := &mockPipeline{}
	sched := NewSyncScheduler(conn, pipe, 100)
	sched.debounceWin = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		sched.Start(ctx)
		close(done)
	}()

	// let it tick for initial sync
	time.Sleep(20 * time.Millisecond)

	// trigger a file event manually
	events <- types.ChangeEvent{Type: "created", Ref: &types.DocumentRef{URI: "test1.md"}}

	time.Sleep(30 * time.Millisecond)

	cancel()

	select {
	case <-done:
		// success
	case <-time.After(1 * time.Second):
		t.Errorf("expected Start to return after context cancellation")
	}
}
