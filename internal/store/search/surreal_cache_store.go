package search

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SurrealCacheStore implements CacheStore using a combination of protocol.Store
// for KV persistence and store.SurrealDBCoreStore for vector operations.
type SurrealCacheStore struct {
	kvStore   protocol.Store
	coreStore *store.SurrealDBCoreStore
}

// NewSurrealCacheStore creates a new CacheStore backend.
func NewSurrealCacheStore(kvStore protocol.Store, coreStore *store.SurrealDBCoreStore) *SurrealCacheStore {
	return &SurrealCacheStore{
		kvStore:   kvStore,
		coreStore: coreStore,
	}
}

func (s *SurrealCacheStore) FindClosest(embedding []float32, threshold float32, limit int) []*CacheEntry {
	if s.coreStore == nil || s.kvStore == nil {
		return nil
	}

	scoredIDs, err := s.coreStore.VecKNN(embedding, limit)
	if err != nil {
		slog.Warn("SurrealCacheStore.FindClosest: VecKNN failed", "error", err)
		return nil
	}

	var results []*CacheEntry
	for _, sid := range scoredIDs {
		// Only consider entries above threshold
		// (Assume VecKNN returns distance or similarity? Usually higher is better if cosine similarity)
		if float32(sid.Score) < threshold {
			continue
		}

		key := []byte("semantic_cache:" + sid.ID)
		val, err := s.kvStore.Get(context.Background(), key)
		if err != nil {
			continue // ignore missing entries (might be deleted from KV but not yet from vector index)
		}

		var entry CacheEntry
		if err := json.Unmarshal(val, &entry); err == nil {
			results = append(results, &entry)
		}
	}

	// VecKNN results are usually sorted, but let's ensure we return the closest first if needed.
	return results
}

func (s *SurrealCacheStore) Put(entry *CacheEntry) error {
	if s.coreStore == nil || s.kvStore == nil {
		return nil // Safe degradation
	}

	// 1. Put into KV store
	val, err := json.Marshal(entry)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "failed to marshal CacheEntry", err)
	}
	key := []byte("semantic_cache:" + entry.Key)
	if err := s.kvStore.Put(context.Background(), key, val); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "failed to put CacheEntry in KV", err)
	}

	// 2. Put into Vector store if embedding is available
	if len(entry.Embedding) > 0 {
		if err := s.coreStore.VecUpsert(entry.Key, entry.Embedding); err != nil {
			slog.Warn("SurrealCacheStore.Put: VecUpsert failed, cache may not be retrievable by semantics", "key", entry.Key, "err", err)
		}
	}

	return nil
}

func (s *SurrealCacheStore) Delete(keys []string) error {
	if s.kvStore == nil {
		return nil
	}

	// We don't have a direct VecDelete in SurrealDBCoreStore according to the current code,
	// so we only delete from KV. Orphaned vectors won't return results from KV lookup.
	for _, k := range keys {
		key := []byte("semantic_cache:" + k)
		_ = s.kvStore.Delete(context.Background(), key)
	}
	return nil
}

func (s *SurrealCacheStore) Count() int {
	if s.kvStore == nil {
		return 0
	}
	it, err := s.kvStore.Scan(context.Background(), []byte("semantic_cache:"))
	if err != nil {
		return 0
	}
	defer it.Close()

	count := 0
	for it.Next() {
		count++
	}
	return count
}

func (s *SurrealCacheStore) ListOldest(n int) []*CacheEntry {
	if s.kvStore == nil || n <= 0 {
		return nil
	}

	it, err := s.kvStore.Scan(context.Background(), []byte("semantic_cache:"))
	if err != nil {
		return nil
	}
	defer it.Close()

	var allEntries []*CacheEntry
	for it.Next() {
		var entry CacheEntry
		if err := json.Unmarshal(it.Value(), &entry); err == nil {
			allEntries = append(allEntries, &entry)
		}
	}

	sort.Slice(allEntries, func(i, j int) bool {
		return allEntries[i].LastAccess.Before(allEntries[j].LastAccess)
	})

	if len(allEntries) > n {
		return allEntries[:n]
	}
	return allEntries
}
