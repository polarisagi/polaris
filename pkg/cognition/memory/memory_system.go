package memory

import (
	"context"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

// ============================================================================
// MemorySystem Facade — Write/Retrieve/Consolidate/Forget
// ============================================================================

// MemorySystemImpl 实现 MemorySystem 接口，作为四层记忆的统一入口。
type MemorySystemImpl struct {
	mem *MemImpl
}

// NewMemorySystem 创建 MemorySystem facade。
func NewMemorySystem(store protocol.Store) *MemorySystemImpl {
	return &MemorySystemImpl{mem: NewMemImpl(store)}
}

// NewMemorySystemWithGraph 创建含 SurrealDB 图路径的 MemorySystem facade（Tier1+）。
func NewMemorySystemWithGraph(store protocol.Store, graph GraphTraverser) *MemorySystemImpl {
	return &MemorySystemImpl{mem: NewMemImplWithGraph(store, graph)}
}

// Mem 返回底层四层 facade。
func (ms *MemorySystemImpl) Mem() protocol.Memory { return ms.mem }

// Write 将 MemoryEntry 路由到对应记忆层。
func (ms *MemorySystemImpl) Write(ctx context.Context, entry *MemoryEntry) error {
	switch entry.Layer {
	case LayerWorking:
		// Working Memory 仅写 ContextWindow（无持久化，热路径）
		ms.mem.working.context.Append(protocol.Message{
			Role:    "assistant",
			Content: entry.Content,
		})
		return nil
	case LayerEpisodic:
		evType := protocol.EventIntent
		if entry.Meta != nil {
			if t, ok := entry.Meta["event_type"].(string); ok && t != "" {
				evType = protocol.EventType(t)
			}
		}
		agentID := ""
		sessionID := entry.ID
		if entry.Meta != nil {
			if v, ok := entry.Meta["agent_id"].(string); ok {
				agentID = v
			}
			if v, ok := entry.Meta["session_id"].(string); ok && v != "" {
				sessionID = v
			}
		}
		ev := protocol.Event{
			ID:        entry.ID,
			Type:      evType,
			TaskID:    sessionID,
			AgentID:   agentID,
			Payload:   []byte(entry.Content),
			CreatedAt: entry.OccurredAt,
		}
		return ms.mem.episodic.Append(ctx, ev)
	case LayerSemantic:
		doc := protocol.Document{
			ID:         entry.ID,
			Title:      entry.ID,
			SourceType: "semantic",
			SourceURI:  entry.Content, // 摘要内容存入 SourceURI
		}
		return ms.mem.semantic.StoreDocument(ctx, doc)
	default:
		// LayerProcedural 由 M6 SkillRegistry 管理，此处不处理
		return nil
	}
}

// Retrieve 通过 HybridRetriever 检索，返回 MemoryEntry 列表。
func (ms *MemorySystemImpl) Retrieve(ctx context.Context, q *RetrievalQuery) ([]MemoryEntry, error) {
	scope := protocol.SearchScope{Type: "memory"}
	if q.Layer == LayerSemantic {
		scope.Type = "semantic"
	}
	config := protocol.RetrievalConfig{
		FinalTopK: q.TopK,
	}
	frags, err := ms.mem.retriever.Search(ctx, q.Text, scope, config)
	if err != nil {
		return nil, err
	}
	var entries []MemoryEntry //nolint:prealloc
	for _, f := range frags {
		entries = append(entries, MemoryEntry{
			ID:         f.Source,
			Layer:      q.Layer,
			Content:    f.Content,
			OccurredAt: time.Now(),
			Meta:       map[string]any{"score": f.Score},
		})
	}
	return entries, nil
}

// Consolidate 触发 Episodic → Semantic 记忆蒸馏。
func (ms *MemorySystemImpl) Consolidate(ctx context.Context) error {
	return ms.mem.episodic.Consolidate(ctx, ms.mem.semantic)
}

// Forget 驱逐超期低质量 Episodic 事件（TTL > 30 天）。
func (ms *MemorySystemImpl) Forget(ctx context.Context) (int, error) {
	ms.mem.episodic.mu.Lock()
	defer ms.mem.episodic.mu.Unlock()

	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	var kept []protocol.Event
	removed := 0
	for _, ev := range ms.mem.episodic.events {
		// CreatedAt 是 time.Time，与 time.Now().Add(-30 days) 比较
		if !ev.CreatedAt.IsZero() && ev.CreatedAt.Before(cutoff) {
			// TTL 超期：从持久化层删除（尽力而为）
			_ = ms.mem.episodic.store.Delete(ctx, []byte("episodic:"+ev.ID))
			removed++
		} else {
			kept = append(kept, ev)
		}
	}
	ms.mem.episodic.events = kept
	return removed, nil
}

// 编译期验证 MemorySystemImpl 实现 MemorySystem 接口
var _ MemorySystem = (*MemorySystemImpl)(nil)
