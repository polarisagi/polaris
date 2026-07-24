package builtin

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// mockCoreMemory 是 protocol.CoreMemory 的内存实现，供 memory_page_out/in 测试使用。
type mockCoreMemory struct {
	mu     sync.Mutex
	blocks map[string]types.CoreMemoryBlock // key: agentID+"|"+sessionID+"|"+blockKey
}

func newMockCoreMemory() *mockCoreMemory {
	return &mockCoreMemory{blocks: make(map[string]types.CoreMemoryBlock)}
}

func (m *mockCoreMemory) key(agentID, sessionID, blockKey string) string {
	return agentID + "|" + sessionID + "|" + blockKey
}

func (m *mockCoreMemory) Get(_ context.Context, agentID, sessionID, blockKey string) (*types.CoreMemoryBlock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.blocks[m.key(agentID, sessionID, blockKey)]
	if !ok {
		return nil, nil
	}
	return &b, nil
}

func (m *mockCoreMemory) Set(_ context.Context, agentID, sessionID, blockKey, content string, taintLevel types.TaintLevel) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blocks[m.key(agentID, sessionID, blockKey)] = types.CoreMemoryBlock{
		AgentID: agentID, SessionID: sessionID, BlockKey: blockKey,
		Content: content, TaintLevel: taintLevel,
	}
	return nil
}

func (m *mockCoreMemory) Delete(_ context.Context, agentID, sessionID, blockKey string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.blocks, m.key(agentID, sessionID, blockKey))
	return nil
}

func (m *mockCoreMemory) List(_ context.Context, agentID, sessionID string) ([]types.CoreMemoryBlock, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []types.CoreMemoryBlock
	for _, b := range m.blocks {
		if b.AgentID == agentID && b.SessionID == sessionID {
			out = append(out, b)
		}
	}
	return out, nil
}

// mockSemanticWriter 是 SemanticMemWriter 的内存实现。
type mockSemanticWriter struct {
	mu       sync.Mutex
	entities map[string]types.Entity // key: type+"|"+name
}

func newMockSemanticWriter() *mockSemanticWriter {
	return &mockSemanticWriter{entities: make(map[string]types.Entity)}
}

func (m *mockSemanticWriter) entKey(entityType, name string) string { return entityType + "|" + name }

func (m *mockSemanticWriter) UpsertFact(_ context.Context, entity types.Entity, _ types.TaintLevel) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entities[m.entKey(entity.Type, entity.Name)] = entity
	return nil
}

// MarkEntityExpired 复核修正（本轮审查）：接口方法由 Archive(ctx, id, reason)
// 改为 MarkEntityExpired(ctx, entityType, name, reason)，此处同步更新 mock 实现
// （见 memory_tools.go SemanticMemWriter 接口定义处的详细说明）。
func (m *mockSemanticWriter) MarkEntityExpired(_ context.Context, entityType, name, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := m.entKey(entityType, name)
	if e, ok := m.entities[k]; ok {
		e.Status = "expired"
		m.entities[k] = e
	}
	return nil
}

func (m *mockSemanticWriter) GetEntity(_ context.Context, entityType, name string) (*types.Entity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entities[m.entKey(entityType, name)]
	if !ok {
		return nil, apperr.New(apperr.CodeNotFound, "entity not found")
	}
	return &e, nil
}

func ctxWithAgentSession(agentID, sessionID string) context.Context {
	ctx := context.WithValue(context.Background(), protocol.CtxAgentIDKey{}, agentID)
	ctx = context.WithValue(ctx, protocol.CtxTaskIDKey{}, sessionID)
	return ctx
}

// TestMemoryPageOutIn_RoundTrip 验证 GD-14-002 最小实现的核心读写正确性：
// 页出后内容从 Core Memory（每轮注入范围）消失，但仍可通过 memory_page_in
// （复用 SemanticMemWriter.GetEntity，等价于"可通过检索找回"）取回；
// 页入后 Core Memory 恢复原内容。
func TestMemoryPageOutIn_RoundTrip(t *testing.T) {
	coreMemory := newMockCoreMemory()
	writer := newMockSemanticWriter()
	ctx := ctxWithAgentSession("agent-1", "session-1")

	// 预置一个 Core Memory 块（模拟 core_memory_edit 此前写入的状态）。
	if err := coreMemory.Set(ctx, "agent-1", "session-1", "subtask_x_details", "detail content here", types.TaintLow); err != nil {
		t.Fatalf("setup Set failed: %v", err)
	}

	pageOutFn := MakeMemoryPageOutFn(coreMemory, writer)
	pageInFn := MakeMemoryPageInFn(coreMemory, writer)

	// Page out。
	outArgs, _ := json.Marshal(map[string]string{"block_key": "subtask_x_details", "reason": "sub-task finished"})
	outResp, err := pageOutFn(ctx, outArgs)
	if err != nil {
		t.Fatalf("memory_page_out failed: %v", err)
	}
	var outResult map[string]string
	if err := json.Unmarshal(outResp, &outResult); err != nil {
		t.Fatalf("unmarshal page_out response failed: %v", err)
	}
	if outResult["status"] != "success" {
		t.Fatalf("expected page_out status=success, got %+v", outResult)
	}

	// 页出后：Core Memory 中应该已消失（不再每轮注入）。
	block, err := coreMemory.Get(ctx, "agent-1", "session-1", "subtask_x_details")
	if err != nil {
		t.Fatalf("Get after page_out failed: %v", err)
	}
	if block != nil {
		t.Fatalf("expected core memory block removed after page_out, still present: %+v", block)
	}

	// 页出后：仍可通过语义记忆找回（等价于 memory_search 可检索到）。
	ent, err := writer.GetEntity(ctx, pagedMemoryEntityType, pagedMemoryEntityName("session-1", "subtask_x_details"))
	if err != nil || ent == nil {
		t.Fatalf("expected paged content retrievable from semantic memory, err=%v ent=%v", err, ent)
	}
	if ent.Properties["content"] != "detail content here" {
		t.Errorf("expected archived content preserved, got %+v", ent.Properties)
	}

	// Page in：恢复到 Core Memory。
	inArgs, _ := json.Marshal(map[string]string{"block_key": "subtask_x_details"})
	inResp, err := pageInFn(ctx, inArgs)
	if err != nil {
		t.Fatalf("memory_page_in failed: %v", err)
	}
	var inResult map[string]string
	if err := json.Unmarshal(inResp, &inResult); err != nil {
		t.Fatalf("unmarshal page_in response failed: %v", err)
	}
	if inResult["status"] != "success" {
		t.Fatalf("expected page_in status=success, got %+v", inResult)
	}

	restored, err := coreMemory.Get(ctx, "agent-1", "session-1", "subtask_x_details")
	if err != nil {
		t.Fatalf("Get after page_in failed: %v", err)
	}
	if restored == nil || restored.Content != "detail content here" {
		t.Fatalf("expected core memory content restored after page_in, got %+v", restored)
	}
}

// TestMemoryPageIn_NotFound 验证未曾 page_out 的 block_key 走软失败路径
// （不是错误，只是 status=not_found），与 memory_expire 的既有约定一致。
func TestMemoryPageIn_NotFound(t *testing.T) {
	coreMemory := newMockCoreMemory()
	writer := newMockSemanticWriter()
	ctx := ctxWithAgentSession("agent-1", "session-1")

	pageInFn := MakeMemoryPageInFn(coreMemory, writer)
	inArgs, _ := json.Marshal(map[string]string{"block_key": "never_paged_out"})
	resp, err := pageInFn(ctx, inArgs)
	if err != nil {
		t.Fatalf("expected nil error for not-found page_in, got: %v", err)
	}
	var result map[string]string
	if err := json.Unmarshal(resp, &result); err != nil {
		t.Fatalf("unmarshal response failed: %v", err)
	}
	if result["status"] != "not_found" {
		t.Errorf("expected status=not_found, got %+v", result)
	}
}

// TestMemoryPageOut_BlockNotFound 验证页出不存在的 block_key 同样走软失败路径。
func TestMemoryPageOut_BlockNotFound(t *testing.T) {
	coreMemory := newMockCoreMemory()
	writer := newMockSemanticWriter()
	ctx := ctxWithAgentSession("agent-1", "session-1")

	pageOutFn := MakeMemoryPageOutFn(coreMemory, writer)
	outArgs, _ := json.Marshal(map[string]string{"block_key": "nonexistent"})
	resp, err := pageOutFn(ctx, outArgs)
	if err != nil {
		t.Fatalf("expected nil error for not-found page_out, got: %v", err)
	}
	var result map[string]string
	if err := json.Unmarshal(resp, &result); err != nil {
		t.Fatalf("unmarshal response failed: %v", err)
	}
	if result["status"] != "not_found" {
		t.Errorf("expected status=not_found, got %+v", result)
	}
}

// TestMemoryPageOutIn_NamespaceIsolation 验证不同 session 的同名 block_key 互不
// 干扰（复合键 sessionID+blockKey，见 pagedMemoryEntityName）。
func TestMemoryPageOutIn_NamespaceIsolation(t *testing.T) {
	coreMemory := newMockCoreMemory()
	writer := newMockSemanticWriter()

	ctxA := ctxWithAgentSession("agent-1", "session-A")
	ctxB := ctxWithAgentSession("agent-1", "session-B")

	if err := coreMemory.Set(ctxA, "agent-1", "session-A", "shared_key", "content A", types.TaintLow); err != nil {
		t.Fatalf("setup A failed: %v", err)
	}
	if err := coreMemory.Set(ctxB, "agent-1", "session-B", "shared_key", "content B", types.TaintLow); err != nil {
		t.Fatalf("setup B failed: %v", err)
	}

	pageOutFn := MakeMemoryPageOutFn(coreMemory, writer)
	args, _ := json.Marshal(map[string]string{"block_key": "shared_key"})

	if _, err := pageOutFn(ctxA, args); err != nil {
		t.Fatalf("page_out A failed: %v", err)
	}

	// session-B 的同名 block 应该完全不受影响。
	blockB, err := coreMemory.Get(ctxB, "agent-1", "session-B", "shared_key")
	if err != nil {
		t.Fatalf("Get B failed: %v", err)
	}
	if blockB == nil || blockB.Content != "content B" {
		t.Fatalf("expected session-B block untouched, got %+v", blockB)
	}
}
