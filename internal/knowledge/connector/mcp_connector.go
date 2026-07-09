package connector

import (
	"context"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// MCPKnowledgeConnector 将具备 knowledge-source 能力的 MCP 客户端适配为 KnowledgeSourceConnector。
// Task 17：知识源插件化接口实现。
// MCPClient defines the required MCP methods for knowledge sync.
type MCPClient interface {
	// TODO: define required MCP SDK methods here when implemented
}

type MCPKnowledgeConnector struct {
	id     string
	name   string
	client MCPClient
}

var _ KnowledgeSourceConnector = (*MCPKnowledgeConnector)(nil)

func NewMCPKnowledgeConnector(id, name string, client MCPClient) *MCPKnowledgeConnector {
	return &MCPKnowledgeConnector{
		id:     id,
		name:   name,
		client: client,
	}
}

func (c *MCPKnowledgeConnector) ID() string {
	return c.id
}

func (c *MCPKnowledgeConnector) Name() string {
	return c.name
}

func (c *MCPKnowledgeConnector) SyncConfig() types.SyncConfig {
	return types.SyncConfig{
		DefaultInterval: 3600,  // 默认 1 小时
		SupportsWatch:   false, // 暂时硬编码为 false，待 MCP SDK 完善 resources_updated 支持
		MaxBatchSize:    100,
	}
}

// List/Fetch 目前仍是桩实现（2026-07-04 审计核实：MCP resources/list、
// resources/read 的真实桥接逻辑尚未接入 mcp.MCPClient，需要先核实该客户端
// 实际暴露的方法签名再补齐，避免臆测接口造成新的错误）。
// 2026-07-04 已修复的部分：此前该 Connector 注册后从未被 SyncScheduler 调度
// （见 registry.go/mcp_installer.go/boot_agent.go 改动），现在已接入调度循环，
// List 每次会按 CodeUnimplemented 快速失败，SyncScheduler 的指数退避重试机制
// 会接住这个错误，不会 panic 或阻塞其它连接器，只是暂时不会真正同步到任何文档。

func (c *MCPKnowledgeConnector) List(ctx context.Context) ([]*types.DocumentRef, error) {
	// 实际应调用 MCP client 的 ResourcesList
	// 暂作桩实现
	return nil, apperr.New(apperr.CodeUnimplemented, "mcp knowledge connector list not fully implemented")
}

func (c *MCPKnowledgeConnector) Fetch(ctx context.Context, ref *types.DocumentRef) (*types.SyncDocument, error) {
	// 实际应调用 MCP client 的 ResourcesRead
	// 暂作桩实现
	return nil, apperr.New(apperr.CodeUnimplemented, "mcp knowledge connector fetch not fully implemented")
}

func (c *MCPKnowledgeConnector) Watch(ctx context.Context) (<-chan types.ChangeEvent, error) {
	if !c.SyncConfig().SupportsWatch {
		return nil, apperr.New(apperr.CodeUnimplemented, "watch not supported")
	}
	return nil, apperr.New(apperr.CodeUnimplemented, "mcp knowledge connector watch not fully implemented")
}
