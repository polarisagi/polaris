package connector

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/knowledge"
	"github.com/polarisagi/polaris/pkg/types"
)

type mockConnector struct {
	events chan types.ChangeEvent
}

func (m *mockConnector) ID() string   { return "mock" }
func (m *mockConnector) Name() string { return "Mock Connector" }
func (m *mockConnector) List(ctx context.Context) ([]*types.DocumentRef, error) {
	return []*types.DocumentRef{{URI: "doc1"}}, nil
}
func (m *mockConnector) Fetch(ctx context.Context, doc *types.DocumentRef) (*types.SyncDocument, error) {
	return &types.SyncDocument{URI: doc.URI, Content: []byte("content")}, nil
}
func (m *mockConnector) Watch(ctx context.Context) (<-chan types.ChangeEvent, error) {
	return m.events, nil
}
func (m *mockConnector) SyncConfig() types.SyncConfig {
	// DefaultInterval=0：测试用连接器不需要周期性全量重同步兜底，
	// 靠 Watch channel 手动喂事件即可覆盖增量路径。
	return types.SyncConfig{SupportsWatch: true}
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

// noWatchConnector 模拟 SupportsWatch=false 的连接器（如 NotionConnector）：
// Watch() 从不产生事件，只能靠 SyncConfig().DefaultInterval 的周期性全量重同步
// 才能在初始 fullSync 之后继续发现变更。
type noWatchConnector struct {
	listCalls int32
}

func (m *noWatchConnector) ID() string   { return "no-watch-mock" }
func (m *noWatchConnector) Name() string { return "No Watch Connector" }
func (m *noWatchConnector) List(ctx context.Context) ([]*types.DocumentRef, error) {
	atomic.AddInt32(&m.listCalls, 1)
	return []*types.DocumentRef{{URI: "doc1"}}, nil
}
func (m *noWatchConnector) Fetch(ctx context.Context, doc *types.DocumentRef) (*types.SyncDocument, error) {
	return &types.SyncDocument{URI: doc.URI, Content: []byte("content")}, nil
}
func (m *noWatchConnector) Watch(ctx context.Context) (<-chan types.ChangeEvent, error) {
	out := make(chan types.ChangeEvent)
	go func() {
		defer close(out)
		<-ctx.Done()
	}()
	return out, nil
}
func (m *noWatchConnector) SyncConfig() types.SyncConfig {
	return types.SyncConfig{SupportsWatch: false, DefaultInterval: 1} // 1s，测试专用极短周期
}

// TestSyncScheduler_PeriodicResyncForNoWatchConnector 验证 P2-4 修复：
// SupportsWatch=false 的连接器不会在初始 fullSync 后彻底停止同步——
// 必须靠 DefaultInterval 驱动的周期性 fullSync 兜底（回归此前 NotionConnector
// 注册后再也不会被重新同步的问题）。
func TestSyncScheduler_PeriodicResyncForNoWatchConnector(t *testing.T) {
	conn := &noWatchConnector{}
	pipe := &mockPipeline{}
	sched := NewSyncScheduler(conn, pipe, 0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = sched.Start(ctx)
		close(done)
	}()

	// 初始 fullSync（同步调用，Start 内部先于进入 select 循环执行）+
	// 至少一次 1s 周期性 resync。
	time.Sleep(1300 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("expected Start to return after context cancellation")
	}

	if calls := atomic.LoadInt32(&conn.listCalls); calls < 2 {
		t.Errorf("expected at least 2 List() calls (initial + periodic resync), got %d", calls)
	}
}
