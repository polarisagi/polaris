package memory

import (
	"context"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// ============================================================================
// MemorySystem Facade — Write/Retrieve/Consolidate/Forget
// ============================================================================

// MemorySystemImpl 实现 protocol.MemorySystem 接口，作为四层记忆的统一入口。
type MemorySystemImpl struct {
	*MemImpl
}

// NewMemorySystemFromMemImpl 创建 MemorySystem facade。
func NewMemorySystemFromMemImpl(mem *MemImpl) *MemorySystemImpl {
	return &MemorySystemImpl{MemImpl: mem}
}

// 2026-07-14（ADR-0051）：NewMemorySystem/NewMemorySystemWithGraph 删除——全仓
// 零调用点，boot_agent.go 唯一使用 NewMemorySystemFromMemImpl 包装已由
// NewMemImplWithDB/NewMemImplFull 构造好的 *MemImpl；本函数是从未被采纳的平行
// 构造路径（NewMemorySystemWithGraph 调用的 NewMemImplWithGraph 本身也是
// 同批删除的幽灵 Tier 档位，见 memory.go）。

// Mem 返回底层四层 facade。
func (ms *MemorySystemImpl) Mem() protocol.MemorySystem { return ms.MemImpl }

// Write 将 MemoryEntry 路由到对应记忆层。
func (ms *MemorySystemImpl) Write(ctx context.Context, entry *MemoryEntry) error {
	switch entry.Layer {
	case LayerWorking:
		// Working Memory 仅写 ContextWindow（无持久化，热路径）
		ms.working.ContextWindow().Append(types.Message{
			Role:    "assistant",
			Content: entry.Content,
		})
		return nil
	case LayerEpisodic:
		evType := types.EventIntent
		if entry.Meta != nil {
			if t, ok := entry.Meta["event_type"].(string); ok && t != "" {
				evType = types.EventType(t)
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
		ev := types.Event{
			ID:        entry.ID,
			Type:      evType,
			TaskID:    sessionID,
			AgentID:   agentID,
			Payload:   []byte(entry.Content),
			CreatedAt: entry.OccurredAt,
		}
		err := ms.episodic.Append(ctx, ev, types.TaintLevel(entry.TaintLevel))
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "MemorySystemImpl.Write: episodic.Append", err)
		}
		return nil
	case LayerSemantic:
		doc := types.Document{
			ID:         entry.ID,
			Title:      entry.ID,
			SourceType: "semantic",
			SourceURI:  entry.Content, // 摘要内容存入 SourceURI
		}
		err := ms.semantic.StoreDocument(ctx, doc, types.TaintLevel(entry.TaintLevel))
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "MemorySystemImpl.Write: semantic.StoreDocument", err)
		}
		return nil
	default:
		// LayerProcedural 由 M6 SkillRegistry 管理，此处不处理
		return nil
	}
}

// List 通过 HybridRetriever 检索，返回 MemoryEntry 列表（R2.2：读多条 → List）。
func (ms *MemorySystemImpl) List(ctx context.Context, q *RetrievalQuery) ([]MemoryEntry, error) {
	scope := types.SearchScope{Type: "memory"}
	if q.Layer == LayerSemantic {
		scope.Type = "semantic"
	}
	config := types.RetrievalConfig{
		FinalTopK: q.TopK,
	}
	frags, err := ms.retriever.Search(ctx, q.Text, scope, config)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "MemorySystemImpl.Retrieve", err)
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

// FlushTrigger 供 Kernel 主动触发归档/固化。
//
// 当前实现说明：working/episodic/semantic/procedural 四层存储的写入路径均为
// 同步落盘（每次 Add/Write 直接写 protocol.Store，无内存缓冲队列），因此本方法
// 目前是安全的 no-op——调用时刻不存在"尚未落盘的脏数据"。已逐个核实
// EpisodicMem.Append（episodic_mem.go）、SemanticMem.StoreDocument/StoreChunks/
// UpsertFact/UpsertRelation（semantic_mem.go）均为同步 store.Put/ExecContext 调用；
// WorkingMem 本身不持有 protocol.Store（纯内存，或经 NotesStore/CoreMemory 同步写
// SQL）；ProceduralMem 无独立写路径，持久化委托给 protocol.SkillRegistry。
// 若未来引入写缓冲/批量提交机制，必须在此处补上真实的 flush 逻辑，
// 且引入前必须先确认调用方（Kernel/FSM）依赖 nil 返回值代表"已确认落盘"
// 这一契约不会被破坏。
func (ms *MemorySystemImpl) FlushTrigger(ctx context.Context) error {
	return nil
}

// InjectRelevantMemory 由内嵌的 *MemImpl 通过方法提升（method promotion）提供，
// 此前这里有一份逐行相同的重复实现（仅错误信息前缀不同），已删除（GR-5-002）。

// Consolidate 触发 Episodic → Semantic 记忆蒸馏。
func (ms *MemorySystemImpl) Consolidate(ctx context.Context) error {
	err := ms.episodic.Consolidate(ctx, ms.semantic)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "MemorySystemImpl.Consolidate", err)
	}
	return nil
}

// Forget 驱逐超期低质量 Episodic 事件（TTL > 30 天）。
func (ms *MemorySystemImpl) Forget(ctx context.Context) (int, error) {
	n, err := ms.episodic.Forget(ctx)
	if err != nil {
		return n, apperr.Wrap(apperr.CodeInternal, "MemorySystemImpl.Forget", err)
	}
	return n, nil
}

// 编译期验证 MemorySystemImpl 实现 protocol.MemorySystem 接口
var _ protocol.MemorySystem = (*MemorySystemImpl)(nil)
