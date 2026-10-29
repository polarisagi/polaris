package native_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/polarisagi/polaris/internal/extension/native"
	"github.com/polarisagi/polaris/internal/knowledge"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// fakeKnowledgeIngester 模拟知识摄入管线用于单测验证。
type fakeKnowledgeIngester struct {
	mu         sync.Mutex
	ingestFunc func(ctx context.Context, doc *knowledge.Document, initialTaint int) (*knowledge.DocTree, error)
	calls      []*knowledge.Document
	taints     []int
}

func (f *fakeKnowledgeIngester) Ingest(ctx context.Context, doc *knowledge.Document, initialTaint int) (*knowledge.DocTree, error) {
	f.mu.Lock()
	f.calls = append(f.calls, doc)
	f.taints = append(f.taints, initialTaint)
	fn := f.ingestFunc
	f.mu.Unlock()

	if fn != nil {
		return fn(ctx, doc, initialTaint)
	}
	return &knowledge.DocTree{
		Document: &knowledge.DocNode{ID: "mock_doc_id", Title: doc.Ref.Title},
	}, nil
}

// TestKnowledgeIngest_SuccessAndMetadata 验证正常单篇摄入、TaintHigh 污点及元数据标记。
func TestKnowledgeIngest_SuccessAndMetadata(t *testing.T) {
	t.Parallel()

	fake := &fakeKnowledgeIngester{}
	fn := native.MakeKnowledgeIngestFn(fake)

	payload := map[string]any{
		"collection": "tech_docs_01",
		"docs": []map[string]any{
			{
				"uri":          "https://polaris.dev/doc/1",
				"title":        "Polaris Architecture",
				"content":      "System architecture details...",
				"source":       "feed",
				"published_at": "2026-10-14T10:00:00Z",
			},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	out, err := fn(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res struct {
		Ingested int `json:"ingested"`
		Failed   []struct {
			URI   string `json:"uri"`
			Error string `json:"error"`
		} `json:"failed"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}

	if res.Ingested != 1 || len(res.Failed) != 0 {
		t.Fatalf("expected 1 ingested, 0 failed, got ingested=%d failed=%d", res.Ingested, len(res.Failed))
	}

	if len(fake.calls) != 1 {
		t.Fatalf("expected 1 ingest call, got %d", len(fake.calls))
	}
	if fake.taints[0] != int(types.TaintHigh) {
		t.Fatalf("expected taint %d (TaintHigh), got %d", types.TaintHigh, fake.taints[0])
	}

	doc := fake.calls[0]
	if doc.Metadata["collection"] != "tech_docs_01" {
		t.Errorf("expected collection tech_docs_01, got %q", doc.Metadata["collection"])
	}
	if doc.Metadata["origin"] != "knowledge_ingest" {
		t.Errorf("expected origin knowledge_ingest, got %q", doc.Metadata["origin"])
	}
	if doc.Metadata["source"] != "feed" {
		t.Errorf("expected source feed, got %q", doc.Metadata["source"])
	}
	if doc.Metadata["published_at"] != "2026-10-14T10:00:00Z" {
		t.Errorf("expected published_at 2026-10-14T10:00:00Z, got %q", doc.Metadata["published_at"])
	}
	if doc.Ref.URI != "https://polaris.dev/doc/1" {
		t.Errorf("expected uri https://polaris.dev/doc/1, got %q", doc.Ref.URI)
	}
	if doc.Ref.Title != "Polaris Architecture" {
		t.Errorf("expected title Polaris Architecture, got %q", doc.Ref.Title)
	}
	if doc.Ref.SourceType != "feed" {
		t.Errorf("expected source_type feed, got %q", doc.Ref.SourceType)
	}
	if doc.Ref.ContentHash == "" {
		t.Errorf("expected non-empty content_hash")
	}
}

// TestKnowledgeIngest_Fallbacks 验证 url -> uri 回退与 excerpt -> content 回退机制。
func TestKnowledgeIngest_Fallbacks(t *testing.T) {
	t.Parallel()

	fake := &fakeKnowledgeIngester{}
	fn := native.MakeKnowledgeIngestFn(fake)

	payload := map[string]any{
		"collection": "fallback_test",
		"docs": []map[string]any{
			{
				"url":     "https://example.com/fallback-url",
				"title":   "Fallback Title",
				"excerpt": "This is an excerpt content.",
			},
		},
	}
	raw, _ := json.Marshal(payload)

	out, err := fn(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res struct {
		Ingested int `json:"ingested"`
		Failed   []any
	}
	_ = json.Unmarshal(out, &res)
	if res.Ingested != 1 {
		t.Fatalf("expected 1 ingested, got %d", res.Ingested)
	}

	if len(fake.calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(fake.calls))
	}
	doc := fake.calls[0]
	if doc.Ref.URI != "https://example.com/fallback-url" {
		t.Errorf("expected uri to fallback to url, got %q", doc.Ref.URI)
	}
	if string(doc.Raw) != "This is an excerpt content." {
		t.Errorf("expected raw to fallback to excerpt, got %q", string(doc.Raw))
	}
}

// TestKnowledgeIngest_Truncation 验证正文超过 200,000 rune 时的截断与 Metadata 标记。
func TestKnowledgeIngest_Truncation(t *testing.T) {
	t.Parallel()

	fake := &fakeKnowledgeIngester{}
	fn := native.MakeKnowledgeIngestFn(fake)

	longContent := strings.Repeat("中", 200010) // 200,010 rune
	payload := map[string]any{
		"collection": "truncate_test",
		"docs": []map[string]any{
			{
				"uri":     "https://example.com/large-doc",
				"content": longContent,
			},
		},
	}
	raw, _ := json.Marshal(payload)

	out, err := fn(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var res struct {
		Ingested int `json:"ingested"`
	}
	_ = json.Unmarshal(out, &res)
	if res.Ingested != 1 {
		t.Fatalf("expected 1 ingested, got %d", res.Ingested)
	}

	doc := fake.calls[0]
	runes := []rune(string(doc.Raw))
	if len(runes) != 200000 {
		t.Fatalf("expected exactly 200000 runes after truncation, got %d", len(runes))
	}
	if doc.Metadata["truncated"] != "1" {
		t.Errorf("expected truncated='1' in metadata, got %q", doc.Metadata["truncated"])
	}
}

// TestKnowledgeIngest_PartialFailure 验证部分文档失败时的错误收集与其余文档持续推进。
func TestKnowledgeIngest_PartialFailure(t *testing.T) {
	t.Parallel()

	fake := &fakeKnowledgeIngester{
		ingestFunc: func(ctx context.Context, doc *knowledge.Document, initialTaint int) (*knowledge.DocTree, error) {
			if doc.Ref.URI == "https://example.com/error" {
				return nil, errors.New("simulated pipeline error")
			}
			return &knowledge.DocTree{}, nil
		},
	}
	fn := native.MakeKnowledgeIngestFn(fake)

	payload := map[string]any{
		"collection": "partial_fail",
		"docs": []map[string]any{
			{"uri": "https://example.com/ok-1", "content": "valid content 1"},
			{"uri": "https://example.com/error", "content": "fail content"},
			{"title": "missing-uri-and-url", "content": "no uri"},
			{"uri": "https://example.com/missing-content"},
			{"uri": "https://example.com/ok-2", "content": "valid content 2"},
		},
	}
	raw, _ := json.Marshal(payload)

	out, err := fn(context.Background(), raw)
	if err != nil {
		t.Fatalf("unexpected top-level error on partial failure: %v", err)
	}

	var res struct {
		Ingested int `json:"ingested"`
		Failed   []struct {
			URI   string `json:"uri"`
			Error string `json:"error"`
		} `json:"failed"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}

	if res.Ingested != 2 {
		t.Errorf("expected 2 ingested, got %d", res.Ingested)
	}
	if len(res.Failed) != 3 {
		t.Fatalf("expected 3 failed items, got %d", len(res.Failed))
	}

	if res.Failed[0].URI != "https://example.com/error" || !strings.Contains(res.Failed[0].Error, "simulated pipeline error") {
		t.Errorf("unexpected failed item 0: %+v", res.Failed[0])
	}
	if res.Failed[1].URI != "" || !strings.Contains(res.Failed[1].Error, "uri or url is required") {
		t.Errorf("unexpected failed item 1: %+v", res.Failed[1])
	}
	if res.Failed[2].URI != "https://example.com/missing-content" || !strings.Contains(res.Failed[2].Error, "content or excerpt is required") {
		t.Errorf("unexpected failed item 2: %+v", res.Failed[2])
	}
}

// TestKnowledgeIngest_ValidationErrors 验证 collection 命名校验、批次数量限制及依赖校验。
func TestKnowledgeIngest_ValidationErrors(t *testing.T) {
	t.Parallel()

	fake := &fakeKnowledgeIngester{}
	fn := native.MakeKnowledgeIngestFn(fake)

	// 1. collection 格式非法
	invalidCollections := []string{
		"",
		"TechDocs",              // 大写字母
		"tech-docs",             // 中划线
		"tech docs",             // 空格
		"tech!",                 // 特殊符号
		strings.Repeat("a", 41), // 超长 (41 > 40)
	}
	for _, col := range invalidCollections {
		payload := map[string]any{
			"collection": col,
			"docs": []map[string]any{
				{"uri": "https://example.com/1", "content": "test"},
			},
		}
		raw, _ := json.Marshal(payload)
		_, err := fn(context.Background(), raw)
		if err == nil {
			t.Errorf("expected error for invalid collection %q, got nil", col)
		} else if apperr.CodeOf(err) != apperr.CodeInvalidInput {
			t.Errorf("expected CodeInvalidInput for collection %q, got %v", col, err)
		}
	}

	// 2. docs 数量为空 (0)
	zeroDocsPayload, _ := json.Marshal(map[string]any{
		"collection": "valid_col",
		"docs":       []map[string]any{},
	})
	if _, err := fn(context.Background(), zeroDocsPayload); err == nil {
		t.Errorf("expected error for empty docs, got nil")
	} else if apperr.CodeOf(err) != apperr.CodeInvalidInput {
		t.Errorf("expected CodeInvalidInput for empty docs, got %v", err)
	}

	// 3. docs 数量超限 (101 条)
	overLimitDocs := make([]map[string]any, 101)
	for i := range overLimitDocs {
		overLimitDocs[i] = map[string]any{"uri": "https://example.com", "content": "c"}
	}
	overDocsPayload, _ := json.Marshal(map[string]any{
		"collection": "valid_col",
		"docs":       overLimitDocs,
	})
	if _, err := fn(context.Background(), overDocsPayload); err == nil {
		t.Errorf("expected error for 101 docs, got nil")
	} else if apperr.CodeOf(err) != apperr.CodeInvalidInput {
		t.Errorf("expected CodeInvalidInput for 101 docs, got %v", err)
	}

	// 4. Ingester 为 nil
	nilFn := native.MakeKnowledgeIngestFn(nil)
	validPayload, _ := json.Marshal(map[string]any{
		"collection": "valid_col",
		"docs":       []map[string]any{{"uri": "https://example.com/1", "content": "test"}},
	})
	if _, err := nilFn(context.Background(), validPayload); err == nil {
		t.Errorf("expected error when ingester is nil, got nil")
	} else if apperr.CodeOf(err) != apperr.CodeInternal {
		t.Errorf("expected CodeInternal for nil ingester, got %v", err)
	}

	// 5. 非法 JSON
	if _, err := fn(context.Background(), []byte("{not json")); err == nil {
		t.Errorf("expected error for invalid JSON, got nil")
	} else if apperr.CodeOf(err) != apperr.CodeInvalidInput {
		t.Errorf("expected CodeInvalidInput for invalid JSON, got %v", err)
	}
}

// TestKnowledgeIngest_ToolMeta 验证嵌入元数据加载与解析成功。
func TestKnowledgeIngest_ToolMeta(t *testing.T) {
	t.Parallel()

	meta, err := native.GetExtensionToolMeta("knowledge_ingest")
	if err != nil {
		t.Fatalf("load knowledge_ingest meta: %v", err)
	}
	if meta.Name != "knowledge_ingest" {
		t.Errorf("expected tool name knowledge_ingest, got %q", meta.Name)
	}
	if meta.Version != "1.0.0" {
		t.Errorf("expected version 1.0.0, got %q", meta.Version)
	}
	if meta.InputSchema == nil {
		t.Errorf("expected non-nil input schema")
	}
}
