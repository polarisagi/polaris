package agentctx

import (
	"context"
	"sort"
	"sync"

	"github.com/polarisagi/polaris/internal/ffi"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/types"
)

type Embedder interface {
	Embed(text string) []float32
}

type ToolStage struct {
	catalog        catalog.Catalog
	embedder       Embedder
	cognitiveStore protocol.CognitiveStore
	threshold      int
	embedCache     map[string][]float32
	embedCacheMu   sync.RWMutex
}

func NewToolStage(catalog catalog.Catalog, embedder Embedder) *ToolStage {
	return &ToolStage{
		catalog:    catalog,
		embedder:   embedder,
		threshold:  40,
		embedCache: make(map[string][]float32),
	}
}

func (s *ToolStage) WithCognitiveStore(store protocol.CognitiveStore) *ToolStage {
	s.cognitiveStore = store
	return s
}

const toolSelectTopK = 20

func (s *ToolStage) SelectFor(ctx context.Context, query string) []types.ToolSchema {
	if s.cognitiveStore != nil {
		if schemas, err := s.cognitiveStore.SearchTools(ctx, query, toolSelectTopK); err == nil && len(schemas) > 0 {
			return schemas
		}
	}

	entries := s.catalog.List(ctx, types.TrustUntrusted)
	if s.embedder == nil || len(entries) <= s.threshold || query == "" {
		return s.toSchemas(entries)
	}

	queryVec := s.embedder.Embed(query)
	if len(queryVec) == 0 {
		return s.toSchemas(entries)
	}

	return s.semanticFilter(queryVec, entries)
}

func (s *ToolStage) semanticFilter(queryVec []float32, entries []protocol.CatalogEntry) []types.ToolSchema {
	type scored struct {
		entry protocol.CatalogEntry
		score float32
	}

	candidates := make([]scored, 0, len(entries))
	for _, e := range entries {
		key := e.Name + "\x00" + e.Description
		vec := s.getOrEmbedTool(key, e.Name+" "+e.Description)
		if len(vec) == 0 {
			candidates = append(candidates, scored{e, 0.5})
			continue
		}
		candidates = append(candidates, scored{e, ffi.VecCosineF32(queryVec, vec)})
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	topK := toolSelectTopK
	if topK > len(candidates) {
		topK = len(candidates)
	}

	result := make([]types.ToolSchema, topK)
	for i := range result {
		result[i] = s.toSchema(candidates[i].entry)
	}
	return result
}

func (s *ToolStage) getOrEmbedTool(key, text string) []float32 {
	s.embedCacheMu.RLock()
	if v, ok := s.embedCache[key]; ok {
		s.embedCacheMu.RUnlock()
		return v
	}
	s.embedCacheMu.RUnlock()

	v := s.embedder.Embed(text)
	if len(v) == 0 {
		return nil
	}

	s.embedCacheMu.Lock()
	if len(s.embedCache) < 1024 {
		s.embedCache[key] = v
	}
	s.embedCacheMu.Unlock()
	return v
}

func (s *ToolStage) toSchemas(entries []protocol.CatalogEntry) []types.ToolSchema {
	schemas := make([]types.ToolSchema, len(entries))
	for i, e := range entries {
		schemas[i] = s.toSchema(e)
	}
	return schemas
}

func (s *ToolStage) toSchema(e protocol.CatalogEntry) types.ToolSchema {
	return types.ToolSchema{
		Name:        e.Name,
		Description: e.Description,
		Parameters:  e.Parameters,
	}
}
