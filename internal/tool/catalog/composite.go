package catalog

import (
	"context"
	"log/slog"
	"sort"
	"sync"

	"github.com/polarisagi/polaris/pkg/types"
)

// CompositeCatalog fan-in 多来源 Catalog，统一去重 + 缓存 + LLM 名合法性检查。
type CompositeCatalog struct {
	mu      sync.RWMutex
	sources []Catalog
	cache   []CatalogEntry // nil 表示需要重建
}

func NewCompositeCatalog(sources ...Catalog) *CompositeCatalog {
	return &CompositeCatalog{
		sources: sources,
	}
}

func isValidLLMName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func (c *CompositeCatalog) filterByTrust(entries []CatalogEntry, minTrust types.TrustTier) []CatalogEntry {
	var filtered []CatalogEntry
	for _, e := range entries {
		if e.TrustTier >= minTrust {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

func (c *CompositeCatalog) List(ctx context.Context, minTrust types.TrustTier) []CatalogEntry {
	c.mu.RLock()
	if c.cache != nil {
		defer c.mu.RUnlock()
		return c.filterByTrust(c.cache, minTrust)
	}
	c.mu.RUnlock()
	return c.rebuild(ctx, minTrust)
}

func (c *CompositeCatalog) rebuild(ctx context.Context, minTrust types.TrustTier) []CatalogEntry {
	seen := make(map[string]bool)
	var result []CatalogEntry
	for _, src := range c.sources {
		for _, e := range src.List(ctx, minTrust) {
			if seen[e.Name] {
				continue
			}
			if !isValidLLMName(e.Name) { // 防御性过滤
				slog.Warn("catalog: invalid LLM tool name, dropped", "name", e.Name)
				continue
			}
			seen[e.Name] = true
			result = append(result, e)
		}
	}

	// Sort by Source (builtin -> mcp -> skill -> other) then by Name
	sourceWeight := func(src types.ToolSource) int {
		switch src {
		case types.ToolBuiltin:
			return 1
		case types.ToolMCP:
			return 2
		case types.ToolSkill:
			return 3
		default:
			return 4
		}
	}
	sort.Slice(result, func(i, j int) bool {
		wi, wj := sourceWeight(result[i].Source), sourceWeight(result[j].Source)
		if wi != wj {
			return wi < wj
		}
		return result[i].Name < result[j].Name
	})

	c.mu.Lock()
	c.cache = result
	c.mu.Unlock()
	return c.filterByTrust(result, minTrust)
}

func (c *CompositeCatalog) Lookup(name string) (CatalogEntry, bool) {
	c.mu.RLock()
	if c.cache != nil {
		defer c.mu.RUnlock()
		for _, e := range c.cache {
			if e.Name == name {
				return e, true
			}
		}
		return CatalogEntry{}, false
	}
	c.mu.RUnlock()

	// If cache is empty, rebuild and try again
	c.rebuild(context.Background(), types.TrustUntrusted)
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, e := range c.cache {
		if e.Name == name {
			return e, true
		}
	}
	return CatalogEntry{}, false
}

func (c *CompositeCatalog) Register(entry CatalogEntry) {
	// Not typically called directly on CompositeCatalog unless we have a default target
	// Or we can just panic/ignore
}

func (c *CompositeCatalog) Unregister(name string) {
	// Same as above
}

func (c *CompositeCatalog) Invalidate() {
	c.mu.Lock()
	c.cache = nil
	c.mu.Unlock()
}

func (c *CompositeCatalog) Schemas(ctx context.Context, minTrust types.TrustTier) []types.ToolSchema {
	entries := c.List(ctx, minTrust)
	schemas := make([]types.ToolSchema, len(entries))
	for i, e := range entries {
		schemas[i] = types.ToolSchema{
			Name:        e.Name,
			Description: e.Description,
			Parameters:  e.Parameters,
		}
	}
	return schemas
}
