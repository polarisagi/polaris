package knowledge

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

// KnowledgeConnector 知识连接器接口（对齐 ObsidianConnector 的实际实现）。
// 与 rag.go 中的本地 Connector 接口不同：此处使用 protocol 包类型，
// 与 ObsidianConnector 的方法签名保持一致。
type KnowledgeConnector interface {
	ID() string
	List(ctx context.Context) ([]*protocol.DocumentRef, error)
	Fetch(ctx context.Context, ref *protocol.DocumentRef) (*protocol.SyncDocument, error)
	Watch(ctx context.Context) (<-chan protocol.ChangeEvent, error)
}

// KnowledgePipeline 摄入管线接口（仅 SyncScheduler 需要的方法子集）。
type KnowledgePipeline interface {
	Ingest(ctx context.Context, doc *Document, initialTaint int) (*DocTree, error)
	Delete(ctx context.Context, uri string) error
}

// SyncScheduler 消费 KnowledgeConnector.Watch 事件并驱动 KnowledgePipeline 保持索引同步。
// 对 created/updated 事件触发 Ingest；对 deleted 事件触发 Delete。
// 设计原则:
//   - 去重防抖：同一 URI 在 debounceWindow 内的多次变更只处理最后一次。
//   - 幂等重试：Ingest 失败时以指数退避重试最多 3 次。
//   - 零阻塞：事件处理在独立 goroutine 运行，不阻塞 Watch channel。
type SyncScheduler struct {
	connector   KnowledgeConnector
	pipeline    KnowledgePipeline
	taintLevel  int
	debounceWin time.Duration
	maxRetry    int

	mu      sync.Mutex
	pending map[string]*pendingEvent // uri → 待处理事件（防抖）
}

type pendingEvent struct {
	evType string
	ref    *protocol.DocumentRef
	fireAt time.Time
}

// NewSyncScheduler 创建同步调度器。
// connector 和 pipeline 必须非 nil；taintLevel 决定摄入文档的初始污染等级。
func NewSyncScheduler(connector KnowledgeConnector, pipeline KnowledgePipeline, taintLevel int) *SyncScheduler {
	return &SyncScheduler{
		connector:   connector,
		pipeline:    pipeline,
		taintLevel:  taintLevel,
		debounceWin: 500 * time.Millisecond,
		maxRetry:    3,
		pending:     make(map[string]*pendingEvent),
	}
}

// Start 启动调度器，阻塞直到 ctx 取消。
// 先执行全量初始索引（幂等），再切入增量 Watch 模式。
func (s *SyncScheduler) Start(ctx context.Context) error {
	// 全量初始索引
	if err := s.fullSync(ctx); err != nil {
		slog.Warn("knowledge: initial full-sync failed", "connector", s.connector.ID(), "err", err)
	}

	events, err := s.connector.Watch(ctx)
	if err != nil {
		return err
	}

	ticker := time.NewTicker(s.debounceWin)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case ev, ok := <-events:
			if !ok {
				return nil
			}
			s.mu.Lock()
			s.pending[ev.Ref.URI] = &pendingEvent{
				evType: ev.Type,
				ref:    ev.Ref,
				fireAt: time.Now().Add(s.debounceWin),
			}
			s.mu.Unlock()

		case now := <-ticker.C:
			s.flushPending(ctx, now)
		}
	}
}

// fullSync 执行全量初始摄入（幂等，已存在则 upsert）。
func (s *SyncScheduler) fullSync(ctx context.Context) error {
	refs, err := s.connector.List(ctx)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if err := s.ingestRef(ctx, ref); err != nil {
			slog.Warn("knowledge: full-sync ingest failed", "uri", ref.URI, "err", err)
		}
	}
	return nil
}

// flushPending 将到期的防抖事件取出并异步处理。
func (s *SyncScheduler) flushPending(ctx context.Context, now time.Time) {
	s.mu.Lock()
	var toProcess []*pendingEvent
	for uri, pe := range s.pending {
		if now.After(pe.fireAt) {
			toProcess = append(toProcess, pe)
			delete(s.pending, uri)
		}
	}
	s.mu.Unlock()

	for _, pe := range toProcess {
		go s.handleEvent(ctx, pe)
	}
}

// handleEvent 处理单个变更事件，含指数退避重试。
func (s *SyncScheduler) handleEvent(ctx context.Context, pe *pendingEvent) {
	delay := 200 * time.Millisecond
	for attempt := range s.maxRetry {
		var err error
		switch pe.evType {
		case "created", "updated":
			err = s.ingestRef(ctx, pe.ref)
		case "deleted":
			err = s.pipeline.Delete(ctx, pe.ref.URI)
		}
		if err == nil {
			return
		}
		slog.Warn("knowledge: sync event failed",
			"type", pe.evType, "uri", pe.ref.URI, "attempt", attempt+1, "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
			delay *= 2
		}
	}
}

// ingestRef 拉取文档内容并摄入到索引。
func (s *SyncScheduler) ingestRef(ctx context.Context, ref *protocol.DocumentRef) error {
	syncDoc, err := s.connector.Fetch(ctx, ref)
	if err != nil {
		return err
	}
	doc := &Document{
		Ref: DocumentRef{
			URI:         ref.URI,
			Title:       ref.Title,
			SourceType:  ref.SourceType,
			ContentHash: ref.ContentHash,
			UpdatedAt:   ref.ModifiedAt,
		},
		Raw: syncDoc.Content,
	}
	if syncDoc.Metadata != nil {
		doc.Metadata = syncDoc.Metadata
	}
	_, err = s.pipeline.Ingest(ctx, doc, s.taintLevel)
	return err
}
