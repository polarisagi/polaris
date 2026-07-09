package catalog

import (
	"context"
	"log/slog"
	"sort"
	"sync"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store/search"
	"github.com/polarisagi/polaris/pkg/types"
)

// CompositeCatalog fan-in 多来源 Catalog，统一去重 + 缓存 + LLM 名合法性检查。
type CompositeCatalog struct {
	mu      sync.RWMutex
	sources []Catalog
	cache   []protocol.CatalogEntry // nil 表示需要重建

	LazyLoadThreshold int
	Embedder          search.Embedder
	activeSessions    map[string]map[string]bool // sessionID -> map[toolName]bool
}

func NewCompositeCatalog(sources ...Catalog) *CompositeCatalog {
	return &CompositeCatalog{
		sources:           sources,
		LazyLoadThreshold: 40, // 默认阈值
		activeSessions:    make(map[string]map[string]bool),
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

func (c *CompositeCatalog) filterByTrust(entries []protocol.CatalogEntry, minTrust types.TrustTier) []protocol.CatalogEntry {
	var filtered []protocol.CatalogEntry
	for _, e := range entries {
		if e.TrustTier >= minTrust {
			filtered = append(filtered, e)
		}
	}
	return filtered
}

func (c *CompositeCatalog) List(ctx context.Context, minTrust types.TrustTier) []protocol.CatalogEntry {
	c.mu.RLock()
	if c.cache != nil {
		defer c.mu.RUnlock()
		return c.filterByTrust(c.cache, minTrust)
	}
	c.mu.RUnlock()
	return c.rebuild(ctx, minTrust)
}

func (c *CompositeCatalog) rebuild(ctx context.Context, minTrust types.TrustTier) []protocol.CatalogEntry {
	seen := make(map[string]bool)
	var result []protocol.CatalogEntry
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

func (c *CompositeCatalog) Lookup(name string) (protocol.CatalogEntry, bool) {
	c.mu.RLock()
	if c.cache != nil {
		defer c.mu.RUnlock()
		for _, e := range c.cache {
			if e.Name == name {
				return e, true
			}
		}
		return protocol.CatalogEntry{}, false
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
	return protocol.CatalogEntry{}, false
}

func (c *CompositeCatalog) Register(entry protocol.CatalogEntry) {
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

// Schemas 构建懒加载后的工具 schema 列表。
//
// TaskID 激活作用域必须与写入端一致：internal/tool/tool_search.go 的
// search_tools 执行路径（经 ExecuteTool → DAG 节点执行，protocol.CtxTaskIDKey
// 由 internal/agent/dag/executor.go 注入）和本方法的读取路径统一使用
// sCtx.SessionID 作为 TaskID 来源（agent_execute.go 的
// executor.Execute(ctx, plan, a.sCtx.SessionID, a.sCtx.AgentID)）。
// 提示词构建阶段的调用方（internal/agent/context/memory_context.go
// BuildPlanContext → BuildToolListSection、internal/agent/fsm/state_machine.go
// 无 Memory 降级路径）均已改为显式传入携带同一 TaskID 的 ctx，search_tools
// 上一轮激活的工具能在下一轮提示词重建时正确出现在 Schemas() 列表里。
// sysadmin 包（internal/gateway/server/sysadmin/tools.go 等）的管理态调用
// 用 context.Background() 是有意为之——这些是无会话概念的一次性管理操作
// （列全部工具供 Admin UI / 工作流校验），不涉及 search_tools 的会话级激活。
func (c *CompositeCatalog) Schemas(ctx context.Context, minTrust types.TrustTier) []types.ToolSchema {
	entries := c.List(ctx, minTrust)
	threshold := c.LazyLoadThreshold
	if threshold <= 0 {
		threshold = 40
	}

	shouldLazyLoad := len(entries) > threshold && c.Embedder != nil

	// 激活作用域 key 必须与 tool_search.go 的 MakeToolSearchFn 一致，
	// 否则 search_tools 激活的工具在这里读不到（见该文件内注释）。
	sessionID := ""
	if tid, ok := ctx.Value(protocol.CtxTaskIDKey{}).(string); ok {
		sessionID = tid
	}

	c.mu.RLock()
	activeMap := c.activeSessions[sessionID]
	c.mu.RUnlock()

	var schemas []types.ToolSchema
	for _, e := range entries {
		isActive := activeMap != nil && activeMap[e.Name]
		// 懒加载模式下，只返回 TrustTier == 4 (core builtin tools) 或者被当前 session 激活的工具
		if shouldLazyLoad && e.TrustTier < 4 && !isActive {
			continue
		}
		schemas = append(schemas, types.ToolSchema{
			Name:        e.Name,
			Description: e.Description,
			Parameters:  e.Parameters,
		})
	}

	if shouldLazyLoad {
		schemas = append(schemas, types.ToolSchema{
			Name:        "search_tools",
			Description: "Search for available tools dynamically based on a query.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Semantic query to find relevant tools",
					},
				},
				"required": []string{"query"},
			},
		})
	}

	return schemas
}

// ActivateTool activates a dynamically discovered tool for the specified session.
func (c *CompositeCatalog) ActivateTool(sessionID string, toolName string) {
	if sessionID == "" || toolName == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.activeSessions == nil {
		c.activeSessions = make(map[string]map[string]bool)
	}
	if c.activeSessions[sessionID] == nil {
		c.activeSessions[sessionID] = make(map[string]bool)
	}
	c.activeSessions[sessionID][toolName] = true
}

// CleanupSession cleans up activated tools when a session ends to prevent memory leaks.
func (c *CompositeCatalog) CleanupSession(sessionID string) {
	if sessionID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.activeSessions != nil {
		delete(c.activeSessions, sessionID)
	}
}
