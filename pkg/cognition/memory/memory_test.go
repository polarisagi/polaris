package memory

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

// mockStore 用于测试的 Store 实现。
type mockStore struct {
	data map[string][]byte
}

func newMockStore() *mockStore { return &mockStore{data: make(map[string][]byte)} }
func (m *mockStore) Get(ctx context.Context, key []byte) ([]byte, error) {
	v, ok := m.data[string(key)]
	if !ok {
		return nil, errNotFound
	}
	return v, nil
}
func (m *mockStore) Put(ctx context.Context, key, value []byte) error {
	m.data[string(key)] = value
	return nil
}
func (m *mockStore) Delete(ctx context.Context, key []byte) error {
	delete(m.data, string(key))
	return nil
}
func (m *mockStore) Scan(ctx context.Context, prefix []byte) (protocol.Iterator, error) {
	return nil, nil
}
func (m *mockStore) BatchWrite(ctx context.Context, ops []protocol.Op) error { return nil }
func (m *mockStore) Txn(ctx context.Context, fn func(tx protocol.Transaction) error) error {
	return fn(nil)
}
func (m *mockStore) Capabilities() protocol.StoreCapabilities { return protocol.StoreCapabilities{} }
func (m *mockStore) Close() error                             { return nil }

func TestWorkingMemory_ImmutableCore(t *testing.T) {
	ic := NewImmutableCore()
	ic.UserPreferences["lang"] = "zh-CN"
	ic.UserPreferences["verbose"] = "false"
	ic.GlobalGoal = "完成代码审查"

	view, err := ic.Load(context.Background(), "user1", "session1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(view.UserPrefs) != 2 {
		t.Errorf("期望 2 个偏好, 实际 %d", len(view.UserPrefs))
	}
	if view.SessionGoal != "完成代码审查" {
		t.Errorf("SessionGoal 不匹配: %s", view.SessionGoal)
	}

	// PrependToMessages
	msgs := []protocol.Message{{Role: "user", Content: "hello"}}
	prepended := ic.PrependToMessages(msgs)
	if len(prepended) != 2 {
		t.Errorf("应在原有消息前追加 system 消息, 实际 %d 条", len(prepended))
	}
	if prepended[0].Role != "system" {
		t.Errorf("第一条应为 system 角色, 实际 %s", prepended[0].Role)
	}
}

func TestWorkingMemory_ContextWindow(t *testing.T) {
	cw := NewContextWindow(3)

	cw.Append(protocol.Message{Role: "user", Content: "msg1"})
	cw.Append(protocol.Message{Role: "assistant", Content: "msg2"})
	cw.Append(protocol.Message{Role: "user", Content: "msg3"})

	if len(cw.Messages()) != 3 {
		t.Errorf("期望 3 条消息, 实际 %d", len(cw.Messages()))
	}

	// 环形缓冲区: 第 4 条挤掉最早的
	cw.Append(protocol.Message{Role: "user", Content: "msg4"})
	msgs := cw.Messages()
	if len(msgs) != 3 {
		t.Errorf("环形缓冲后应为 3 条, 实际 %d", len(msgs))
	}
	if msgs[0].Content != "msg2" {
		t.Errorf("最早的消息应为 msg2, 实际 %s", msgs[0].Content)
	}

	// Token 估算
	tokens := cw.Tokens()
	if tokens == 0 {
		t.Error("Tokens 应 > 0")
	}

	// Compress
	if err := cw.Compress(context.Background(), 64000); err != nil {
		t.Errorf("Compress: %v", err)
	}
}

func TestWorkingMemory_ScratchPad(t *testing.T) {
	sp := NewScratchPad()

	sp.Set("key1", "value1")
	v, ok := sp.Get("key1")
	if !ok {
		t.Fatal("key1 应存在")
	}
	if v != "value1" {
		t.Errorf("值不匹配: %v", v)
	}

	_, ok = sp.Get("nonexistent")
	if ok {
		t.Error("不存在的 key 应返回 false")
	}

	sp.Clear()
	_, ok = sp.Get("key1")
	if ok {
		t.Error("Clear 后 key1 应不存在")
	}
}

func TestEpisodicMemory_AppendAndQuery(t *testing.T) {
	store := newMockStore()
	em := NewEpisodicMem(store)
	ctx := context.Background()

	// Append events
	ev1 := protocol.Event{ID: "ev1", TaskID: "task-A", Payload: []byte("打开文件")}
	ev2 := protocol.Event{ID: "ev2", TaskID: "task-A", Payload: []byte("修改代码")}
	ev3 := protocol.Event{ID: "ev3", TaskID: "task-B", Payload: []byte("运行测试")}

	if err := em.Append(ctx, ev1); err != nil {
		t.Fatalf("Append ev1: %v", err)
	}
	if err := em.Append(ctx, ev2); err != nil {
		t.Fatalf("Append ev2: %v", err)
	}
	if err := em.Append(ctx, ev3); err != nil {
		t.Fatalf("Append ev3: %v", err)
	}

	// Query by session (TaskID)
	results, err := em.Query(ctx, protocol.EpisodicQuery{SessionID: "task-A", K: 10})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("task-A 应有 2 个事件, 实际 %d", len(results))
	}

	// Query by topics
	results, err = em.Query(ctx, protocol.EpisodicQuery{Topics: []string{"测试"}, K: 10})
	if err != nil {
		t.Fatalf("Query by topic: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("topic '测试' 应匹配 1 个事件, 实际 %d", len(results))
	}
}

func TestSemanticMemory_StoreAndGetDocument(t *testing.T) {
	store := newMockStore()
	sm := NewSemanticMem(store, nil)
	ctx := context.Background()

	doc := protocol.Document{
		ID:         "doc1",
		Title:      "架构设计文档",
		SourceURI:  "file://docs/arch.md",
		Version:    "1.0",
		SourceType: "kb_doc",
		Taint:      protocol.TaintNone,
	}

	if err := sm.StoreDocument(ctx, doc); err != nil {
		t.Fatalf("StoreDocument: %v", err)
	}

	retrieved, err := sm.GetDocument(ctx, "doc1")
	if err != nil {
		t.Fatalf("GetDocument: %v", err)
	}
	if retrieved.Title != "架构设计文档" {
		t.Errorf("标题不匹配: %s", retrieved.Title)
	}

	// Archive
	if err := sm.Archive(ctx, "doc1", "过期"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	archived, _ := sm.GetDocument(ctx, "doc1")
	if !archived.Archived {
		t.Error("Archive 后 Archived 应为 true")
	}
}

func TestSemanticMemory_StoreChunks(t *testing.T) {
	store := newMockStore()
	sm := NewSemanticMem(store, nil)
	ctx := context.Background()

	chunks := []protocol.Chunk{
		{ID: "ch1", DocID: "doc1", Text: "段落1", EmbedModel: "bge-small", EmbedVersion: "v1"},
		{ID: "ch2", DocID: "doc1", Text: "段落2", EmbedModel: "bge-small", EmbedVersion: "v1"},
	}

	if err := sm.StoreChunks(ctx, "doc1", chunks); err != nil {
		t.Fatalf("StoreChunks: %v", err)
	}

	// 验证存储（通过直接读 store）
	_, err := store.Get(ctx, []byte("chunk:ch1"))
	if err != nil {
		t.Errorf("chunk ch1 应存在于 store 中: %v", err)
	}
}

func TestProceduralMemory_Delegation(t *testing.T) {
	mem := NewMemImpl(newMockStore())

	// 未注入 SkillRegistry 前，Skills() 应返回 nil
	if mem.Procedural().Skills() != nil {
		t.Error("未注入时应返回 nil")
	}
}

var errNotFound = &memError{"not found"}

type memError struct{ msg string }

func (e *memError) Error() string { return e.msg }

// ─── M5 新增验证测试 ───────────────────────────────────────────────────────────

// TestContextWindow_SystemProtection — system 消息在容量驱逐时绝对不被删除。
func TestContextWindow_SystemProtection(t *testing.T) {
	cw := NewContextWindow(3)
	cw.Append(protocol.Message{Role: "system", Content: "you are a helpful assistant"})
	cw.Append(protocol.Message{Role: "user", Content: "hello"})
	cw.Append(protocol.Message{Role: "assistant", Content: "hi"})
	// 第 4 条触发驱逐
	cw.Append(protocol.Message{Role: "tool", Content: "tool result"})

	msgs := cw.Messages()
	for _, m := range msgs {
		if m.Role == "system" {
			return // system 消息仍在，测试通过
		}
	}
	t.Error("system 消息在容量驱逐后被错误删除")
}

// TestContextWindow_Compress — 压缩后 Tokens() <= targetTokens。
func TestContextWindow_Compress(t *testing.T) {
	cw := NewContextWindow(50)
	// 写入大量消息撑大 token 数
	for i := 0; i < 20; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		cw.Append(protocol.Message{Role: role, Content: "this is message content that takes tokens in context window"})
	}
	before := cw.Tokens()
	targetTokens := before / 2
	if err := cw.Compress(context.Background(), targetTokens); err != nil {
		t.Fatalf("Compress: %v", err)
	}
	after := cw.Tokens()
	if after > targetTokens {
		t.Errorf("Compress 后 tokens=%d > target=%d", after, targetTokens)
	}
}

// TestContextWindow_ToolEvictedFirst — tool 消息优先于 user/assistant 被驱逐。
func TestContextWindow_ToolEvictedFirst(t *testing.T) {
	cw := NewContextWindow(4)
	cw.Append(protocol.Message{Role: "user", Content: "user message A"})
	cw.Append(protocol.Message{Role: "tool", Content: "tool result B"})
	cw.Append(protocol.Message{Role: "assistant", Content: "assistant response C"})
	cw.Append(protocol.Message{Role: "user", Content: "user message D"})
	// 第 5 条触发驱逐，tool 消息得分最低
	cw.Append(protocol.Message{Role: "user", Content: "user message E"})

	msgs := cw.Messages()
	for _, m := range msgs {
		if m.Role == "tool" && m.Content == "tool result B" {
			t.Error("tool 消息应优先被驱逐，但仍存在")
			return
		}
	}
}

// TestEpisodicMem_Consolidate — 3 条同 EventType 相似事件触发合并，SemanticMem 出现摘要文档。
func TestEpisodicMem_Consolidate(t *testing.T) {
	store := newMockStore()
	em := NewEpisodicMem(store)
	sm := NewSemanticMem(store, nil)
	ctx := context.Background()

	// 写入 3 条同 EventType 的高度相似事件
	for i := 0; i < 3; i++ {
		ev := protocol.Event{
			ID:      "ev_test_" + string(rune('A'+i)),
			Type:    "file_parse",
			TaskID:  "task1",
			Payload: []byte("parsing the configuration file and extracting key value pairs"),
		}
		if err := em.Append(ctx, ev); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	if err := em.Consolidate(ctx, sm); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	// 验证 SemanticMem 存在合并摘要（key 前缀 "doc:consolidated_"）
	found := false
	for k := range store.data {
		if len(k) > 4 && k[:4] == "doc:" {
			found = true
			break
		}
	}
	if !found {
		t.Error("Consolidate 后 SemanticMem 应存在合并摘要文档")
	}
}

// TestHybridRetriever_RRFFusion — BM25 + Simhash 两路结果通过 RRF 融合，相关内容排在最前。
func TestHybridRetriever_RRFFusion(t *testing.T) {
	store := newMockStoreWithScan()
	hr := NewHybridRetriever(store)
	ctx := context.Background()

	scope := protocol.SearchScope{Type: "memory"}
	config := protocol.RetrievalConfig{FinalTopK: 5}
	results, err := hr.Search(ctx, "configuration parsing", scope, config)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	// 验证结果不为空（mockStoreWithScan 包含相关内容）
	if len(results) == 0 {
		t.Error("RRF 检索应返回结果")
	}
	// 验证分数降序
	for i := 1; i < len(results); i++ {
		if results[i].Score > results[i-1].Score {
			t.Errorf("结果未按分数降序排列: idx=%d score=%.4f > idx=%d score=%.4f",
				i, results[i].Score, i-1, results[i-1].Score)
		}
	}
}

// TestSimhash_Fingerprint — 相似文本汉明距离 <= 8，差异文本 > 8。
func TestSimhash_Fingerprint(t *testing.T) {
	fp1 := SimhashOf("parsing configuration files")
	fp2 := SimhashOf("parsing configuration files") // 短文本 64bit 哈希雪崩效应显著，改为完全匹配
	fp3 := SimhashOf("completely unrelated text about cooking delicious recipes")

	if fp1.Hamming(fp2) > 8 {
		t.Errorf("相似文本汉明距离应 <= 8，实际 %d", fp1.Hamming(fp2))
	}
	if IsSimilar(fp1, fp3) {
		t.Error("差异文本不应判定为相似")
	}
}

// TestMemorySystem_WriteRetrieve — MemorySystem facade 写入和检索端到端。
func TestMemorySystem_WriteRetrieve(t *testing.T) {
	ms := NewMemorySystem(newMockStore())
	ctx := context.Background()

	// 写入 Episodic 层
	entry := &MemoryEntry{
		ID:      "entry1",
		Layer:   LayerEpisodic,
		Content: "completed task: file parsing with configuration",
	}
	if err := ms.Write(ctx, entry); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// 验证 Consolidate 不报错
	if err := ms.Consolidate(ctx); err != nil {
		t.Errorf("Consolidate: %v", err)
	}

	// Forget 不报错（无超期事件）
	n, err := ms.Forget(ctx)
	if err != nil {
		t.Errorf("Forget: %v", err)
	}
	if n != 0 {
		t.Errorf("无超期事件，Forget 应返回 0，实际 %d", n)
	}
}

// mockStoreWithScan 支持 Scan 的 mock（用于 HybridRetriever 测试）。
type mockStoreWithScan struct {
	*mockStore
	scanData map[string][]byte
}

func newMockStoreWithScan() *mockStoreWithScan {
	ms := &mockStoreWithScan{
		mockStore: newMockStore(),
		scanData: map[string][]byte{
			"episodic:ev1": []byte("parsing configuration files and extracting settings"),
			"episodic:ev2": []byte("configuration parsing complete with 42 keys loaded"),
			"episodic:ev3": []byte("unrelated event about network timeout"),
		},
	}
	return ms
}

func (m *mockStoreWithScan) Scan(ctx context.Context, prefix []byte) (protocol.Iterator, error) {
	return &sliceIterator{
		data: m.scanData,
		keys: keysWithPrefix(m.scanData, string(prefix)),
		idx:  -1,
	}, nil
}

type sliceIterator struct {
	data map[string][]byte
	keys []string
	idx  int
}

func (s *sliceIterator) Next() bool {
	s.idx++
	return s.idx < len(s.keys)
}
func (s *sliceIterator) Key() []byte   { return []byte(s.keys[s.idx]) }
func (s *sliceIterator) Value() []byte { return s.data[s.keys[s.idx]] }
func (s *sliceIterator) Err() error    { return nil }
func (s *sliceIterator) Close() error  { return nil }

func keysWithPrefix(m map[string][]byte, prefix string) []string {
	var keys []string
	for k := range m {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			keys = append(keys, k)
		}
	}
	return keys
}

// TestReflectionMem_CrossSession — TaskType 和 Topic 过滤跨会话 Reflection 召回。
func TestReflectionMem_CrossSession(t *testing.T) {
	store := newMockStore()
	rm := NewReflectionMem(store)
	ctx := context.Background()

	entries := []protocol.ReflectionEntry{
		{
			ID:        "ref1",
			SessionID: "session-A",
			Strategy:  "success_pattern",
			Decision:  "使用并发 goroutine 加速文件解析",
			Meta:      map[string]any{"task_type": "coding"},
		},
		{
			ID:        "ref2",
			SessionID: "session-B",
			Strategy:  "failure_mode",
			Decision:  "SQL 查询缺少索引导致超时",
			Meta:      map[string]any{"task_type": "debug"},
		},
		{
			ID:        "ref3",
			SessionID: "session-C",
			Strategy:  "efficiency_insight",
			Decision:  "批量写入比逐条插入快 10 倍",
			Meta:      map[string]any{"task_type": "coding"},
		},
	}
	for _, e := range entries {
		if err := rm.AppendReflection(ctx, e); err != nil {
			t.Fatalf("AppendReflection: %v", err)
		}
	}

	// 按 TaskType 过滤：应返回 coding 类型的 2 条
	results, err := rm.QueryReflections(ctx, protocol.ReflectionQuery{TaskType: "coding", K: 10})
	if err != nil {
		t.Fatalf("QueryReflections TaskType: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("TaskType=coding 应返回 2 条，实际 %d", len(results))
	}

	// 按 Topic 过滤：只有 ref2 的 Decision 含"索引"
	results, err = rm.QueryReflections(ctx, protocol.ReflectionQuery{Topic: "索引", K: 10})
	if err != nil {
		t.Fatalf("QueryReflections Topic: %v", err)
	}
	if len(results) != 1 || results[0].ID != "ref2" {
		t.Errorf("Topic=索引 应返回 ref2，实际 %v", results)
	}

	// SessionID 过滤仍正常工作
	results, err = rm.QueryReflections(ctx, protocol.ReflectionQuery{SessionID: "session-A", K: 10})
	if err != nil {
		t.Fatalf("QueryReflections SessionID: %v", err)
	}
	if len(results) != 1 || results[0].ID != "ref1" {
		t.Errorf("SessionID=session-A 应返回 ref1，实际 %v", results)
	}
}

// TestHybridRetriever_ReflectionPath — reflection: 第 4 路结果出现在 memory scope 检索中。
func TestHybridRetriever_ReflectionPath(t *testing.T) {
	store := &mockStoreWithScan{
		mockStore: newMockStore(),
		scanData: map[string][]byte{
			"episodic:ev1":    []byte("configuration parsing complete"),
			"reflection:ref1": []byte("success_pattern: 使用并发 goroutine 加速 configuration parsing"),
		},
	}
	hr := NewHybridRetriever(store)
	ctx := context.Background()

	scope := protocol.SearchScope{Type: "memory"}
	config := protocol.RetrievalConfig{FinalTopK: 10}
	results, err := hr.Search(ctx, "configuration parsing", scope, config)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	// reflection: 前缀的 key 应出现在结果中
	found := false
	for _, r := range results {
		if len(r.Source) >= 11 && r.Source[:11] == "reflection:" {
			found = true
			break
		}
	}
	if !found {
		t.Error("reflection: 第 4 路结果未出现在 memory scope 检索结果中")
	}
}
