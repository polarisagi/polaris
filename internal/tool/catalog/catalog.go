package catalog

import (
	"context"
	"time"

	"github.com/polarisagi/polaris/pkg/types"
)

// CatalogEntry 工具目录条目（所有来源统一格式）。
type CatalogEntry struct {
	Name        string
	Description string
	Parameters  any              // JSON Schema
	Source      types.ToolSource // builtin / mcp / skill / native
	Capability  types.CapabilityLevel
	TrustTier   types.TrustTier
	TaintLevel  types.TaintLevel
	Timeout     time.Duration
	// 执行路由所需元数据
	MCPServerID string // Source==ToolMCP 时有效
	MCPToolName string // MCP 协议原始工具名（非 LLM 调用名）
	SkillName   string // Source==ToolSkill 时有效（"skill:xxx" 格式）
}

// Catalog 统一工具目录接口（单一 Schema 来源）。
type Catalog interface {
	// List 返回所有满足最低信任等级的工具 Schema。
	List(ctx context.Context, minTrust types.TrustTier) []CatalogEntry
	// Lookup 按 LLM 调用名查找。
	Lookup(name string) (CatalogEntry, bool)
	// Register 注册/覆盖（builtin 启动注册；MCP 连接后热注册）。
	Register(entry CatalogEntry)
	// Unregister 移除（MCP 断开时）。
	Unregister(name string)
	// Invalidate 清除 schema 缓存（下次 List 重建）。
	Invalidate()
	// Schemas 返回 []types.ToolSchema，供注入 InferRequest。
	Schemas(ctx context.Context, minTrust types.TrustTier) []types.ToolSchema
}
