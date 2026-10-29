package catalog

import (
	"context"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// protocol.CatalogEntry 工具目录条目（所有来源统一格式）。

// Catalog 统一工具目录接口（单一 Schema 来源）。
type Catalog interface {
	// List 返回所有满足最低信任等级的工具 Schema。
	List(ctx context.Context, minTrust types.TrustTier) []protocol.CatalogEntry
	// Lookup 按 LLM 调用名查找。
	Lookup(name string) (protocol.CatalogEntry, bool)
	// Register 注册/覆盖（builtin 启动注册；MCP 连接后热注册）。
	Register(entry protocol.CatalogEntry)
	// Unregister 移除（MCP 断开时）。
	Unregister(name string)
	// Invalidate 清除 schema 缓存（下次 List 重建）。
	Invalidate()
	// Schemas 返回 []types.ToolSchema，供注入 InferRequest。
	Schemas(ctx context.Context, minTrust types.TrustTier) []types.ToolSchema
}
