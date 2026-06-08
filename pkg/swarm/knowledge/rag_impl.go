package knowledge

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate"
)

// DefaultIngestionPipeline 实现了 IngestionPipeline，负责分块与打标污染等级
type DefaultIngestionPipeline struct {
	router *substrate.StorageRouter
}

func NewDefaultIngestionPipeline(router *substrate.StorageRouter) *DefaultIngestionPipeline {
	return &DefaultIngestionPipeline{
		router: router,
	}
}

func (p *DefaultIngestionPipeline) Ingest(ctx context.Context, doc *Document, initialTaint int) (*DocTree, error) {
	if doc == nil {
		return nil, perrors.New(perrors.CodeInvalidInput, "document is nil")
	}

	docNode := &DocNode{
		ID:      fmt.Sprintf("doc_%s_%d", doc.Ref.ContentHash, time.Now().UnixNano()),
		Title:   doc.Ref.Title,
		Level:   0,
		Content: string(doc.Raw),
	}

	tree := &DocTree{
		Document:   docNode,
		SourceURL:  doc.Ref.URI,
		SourcePath: doc.Ref.URI,
	}

	chunks := p.chunkDocument(docNode.Content, docNode.ID, initialTaint)

	store := p.router.Route(ctx, &substrate.StorageRequest{
		DataType:   "knowledge",
		AccessMode: "batch_write",
	})

	var ops []protocol.Op //nolint:prealloc
	for _, c := range chunks {
		data, _ := json.Marshal(c)
		ops = append(ops, protocol.Op{
			Key:   fmt.Appendf(nil, "chunk:%s:%s", docNode.ID, c.ID),
			Value: data,
			Type:  protocol.OpPut,
		})
	}

	docData, _ := json.Marshal(tree)
	ops = append(ops, protocol.Op{
		Key:   fmt.Appendf(nil, "doc:%s", docNode.ID),
		Value: docData,
		Type:  protocol.OpPut,
	})

	if err := store.BatchWrite(ctx, ops); err != nil {
		return nil, perrors.Wrap(perrors.CodeInternal, "failed to persist document and chunks", err)
	}

	return tree, nil
}

func (p *DefaultIngestionPipeline) Delete(ctx context.Context, uri string) error {
	store := p.router.Route(ctx, &substrate.StorageRequest{
		DataType:   "knowledge",
		AccessMode: "batch_write",
	})

	prefix := fmt.Appendf(nil, "chunk:%s:", uri)
	iter, err := store.Scan(ctx, prefix)
	if err != nil {
		return err
	}
	defer iter.Close()

	var ops []protocol.Op
	for iter.Next() {
		ops = append(ops, protocol.Op{
			Key:  iter.Key(),
			Type: protocol.OpDelete,
		})
	}

	ops = append(ops, protocol.Op{
		Key:  fmt.Appendf(nil, "doc:%s", uri),
		Type: protocol.OpDelete,
	})

	return store.BatchWrite(ctx, ops)
}

func (p *DefaultIngestionPipeline) chunkDocument(content string, docID string, taintLevel int) []Chunk {
	var chunks []Chunk

	// 简单实现：按 1000 字符切分为 ParentChunk，按 250 字符切分为 LeafChunk
	parentSize := 1000
	leafSize := 250

	runes := []rune(content)

	for i := 0; i < len(runes); i += parentSize {
		end := min(i+parentSize, len(runes))

		parentChunkID := fmt.Sprintf("pchunk_%s_%d", docID, i)
		parentChunk := Chunk{
			ID:          parentChunkID,
			Content:     string(runes[i:end]),
			DocID:       docID,
			SectionPath: []string{"root"},
			TaintLevel:  taintLevel,
			TaintSource: "ingestion",
		}
		chunks = append(chunks, parentChunk)

		// 对 ParentChunk 进一步切分为 LeafChunk
		for j := i; j < end; j += leafSize {
			leafEnd := min(j+leafSize, end)
			leafChunkID := fmt.Sprintf("lchunk_%s_%d", docID, j)
			leafChunk := Chunk{
				ID:            leafChunkID,
				Content:       string(runes[j:leafEnd]),
				DocID:         docID,
				SectionPath:   []string{"root", parentChunkID},
				ParentChunkID: parentChunkID,
				TaintLevel:    taintLevel, // 污染标记传递，防止 Taint Washing
				TaintSource:   "ingestion",
			}
			chunks = append(chunks, leafChunk)
		}
	}

	return chunks
}

// DefaultHybridRetriever 实现了 HybridRetriever
type DefaultHybridRetriever struct {
	engine *substrate.HybridSearchEngine
}

func NewDefaultHybridRetriever(router *substrate.StorageRouter, embedder substrate.Embedder) *DefaultHybridRetriever {
	return &DefaultHybridRetriever{
		engine: substrate.NewHybridSearchEngine(router, embedder),
	}
}

func (r *DefaultHybridRetriever) Search(ctx context.Context, query *SearchQuery) ([]Chunk, error) {
	if query == nil || query.Text == "" {
		return nil, perrors.New(perrors.CodeInvalidInput, "empty query")
	}

	config := substrate.RetrievalConfig{
		BM25Weight:   0.3,
		VectorWeight: 0.6,
		GraphWeight:  0.1,
		RRFK:         60,
		OversampleN:  3,
		RerankTopM:   50,
		FinalTopK:    query.TopK,
	}
	if config.FinalTopK <= 0 {
		config.FinalTopK = 5
	}

	fragments, err := r.engine.Search(ctx, query.Text, []byte("chunk:"), config)
	if err != nil {
		return nil, err
	}

	var finalResults []Chunk //nolint:prealloc
	for _, f := range fragments {
		finalResults = append(finalResults, Chunk{
			ID:      f.Source,
			Content: f.Content,
		})
	}

	return finalResults, nil
}
