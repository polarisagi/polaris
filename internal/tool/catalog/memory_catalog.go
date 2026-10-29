package catalog

import (
	"context"
	"sync"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type MemoryCatalog struct {
	mu      sync.RWMutex
	entries map[string]protocol.CatalogEntry
}

func NewMemoryCatalog() *MemoryCatalog {
	return &MemoryCatalog{
		entries: make(map[string]protocol.CatalogEntry),
	}
}

func (m *MemoryCatalog) List(ctx context.Context, minTrust types.TrustTier) []protocol.CatalogEntry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []protocol.CatalogEntry
	for _, e := range m.entries {
		if e.TrustTier >= minTrust {
			result = append(result, e)
		}
	}
	return result
}

func (m *MemoryCatalog) Lookup(name string) (protocol.CatalogEntry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.entries[name]
	return e, ok
}

func (m *MemoryCatalog) Register(entry protocol.CatalogEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[entry.Name] = entry
}

func (m *MemoryCatalog) Unregister(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, name)
}

func (m *MemoryCatalog) Invalidate() {
	// Memory catalog is source of truth, invalidate means nothing.
}

func (m *MemoryCatalog) Schemas(ctx context.Context, minTrust types.TrustTier) []types.ToolSchema {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var result []types.ToolSchema
	for _, e := range m.entries {
		if e.TrustTier >= minTrust {
			result = append(result, types.ToolSchema{
				Name:        e.Name,
				Description: e.Description,
				Parameters:  e.Parameters,
			})
		}
	}
	return result
}
