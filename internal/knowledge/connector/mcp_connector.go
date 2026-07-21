package connector

import (
	"context"
	"encoding/base64"
	"strings"

	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// MCPKnowledgeConnector 将具备 knowledge-source 能力的 MCP 客户端适配为 KnowledgeSourceConnector。
// Task 17：知识源插件化接口实现。
// MCPClient 只声明本文件实际调用的两个方法（消费方定义接口，HE-3/R1.4），
// 由 *mcp.MCPClient 结构性满足，调用方（mcp_installer.go）无需引入具体类型依赖。
type MCPClient interface {
	ResourcesList(ctx context.Context) ([]mcp.MCPResource, error)
	ResourcesRead(ctx context.Context, uri string) ([]mcp.MCPResourceContent, error)
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

// List/Fetch 2026-07-21 deadcode 审查补齐：接入 mcp.MCPClient 的
// ResourcesList/ResourcesRead 真实 RPC 调用（MCP resources/list、
// resources/read，见 mcp_client_protocol.go）。此前该 Connector 注册后从未被
// SyncScheduler 调度（见 registry.go/mcp_installer.go/boot_agent.go 改动），
// 现在已接入调度循环；本次是补上真正的资源桥接逻辑本身。

func (c *MCPKnowledgeConnector) List(ctx context.Context) ([]*types.DocumentRef, error) {
	resources, err := c.client.ResourcesList(ctx)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mcp knowledge connector: resources/list failed", err)
	}
	refs := make([]*types.DocumentRef, 0, len(resources))
	for _, r := range resources {
		refs = append(refs, &types.DocumentRef{
			URI:        r.URI,
			Title:      r.Name,
			SourceType: mimeTypeToSourceType(r.MIMEType),
			Metadata: map[string]any{
				"description": r.Description,
				"mime_type":   r.MIMEType,
			},
		})
	}
	return refs, nil
}

func (c *MCPKnowledgeConnector) Fetch(ctx context.Context, ref *types.DocumentRef) (*types.SyncDocument, error) {
	contents, err := c.client.ResourcesRead(ctx, ref.URI)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "mcp knowledge connector: resources/read failed", err)
	}
	if len(contents) == 0 {
		return nil, apperr.New(apperr.CodeInternal, "mcp knowledge connector: resources/read returned no content for "+ref.URI)
	}
	// MCP spec 允许一次读取返回多个内容块（如资源被拆分为多段）；知识摄入管线
	// 消费单一文档正文，按顺序拼接为一份文本。二进制块（blob）解码失败时跳过，
	// 不中断整体 Fetch（与 CallToolTainted 里 image block 解码失败的降级策略一致）。
	var sb strings.Builder
	mimeType := ""
	for _, ct := range contents {
		if mimeType == "" {
			mimeType = ct.MIMEType
		}
		if ct.Text != "" {
			sb.WriteString(ct.Text)
			continue
		}
		if ct.Blob != "" {
			raw, decErr := base64.StdEncoding.DecodeString(ct.Blob)
			if decErr != nil {
				continue
			}
			sb.Write(raw)
		}
	}
	return &types.SyncDocument{
		URI:      ref.URI,
		Title:    ref.Title,
		Content:  []byte(sb.String()),
		Metadata: map[string]string{"mime_type": mimeType},
	}, nil
}

// mimeTypeToSourceType 把 MCP 资源的 MIME type 映射到 internal/knowledge 摄入
// 管线识别的 SourceType（决定分块策略，见 ingester.go switch doc.Ref.SourceType）。
// 映射不精确时安全退化到 PlainTextChunker（ingester.go default 分支），不是
// 正确性风险，只影响分块粒度。
func mimeTypeToSourceType(mimeType string) string {
	switch {
	case strings.Contains(mimeType, "markdown"):
		return "markdown"
	case strings.Contains(mimeType, "pdf"):
		return "pdf"
	case strings.HasPrefix(mimeType, "text/x-") || strings.Contains(mimeType, "code"):
		return "code"
	case strings.HasPrefix(mimeType, "text/html"):
		return "web"
	default:
		return "" // 落到 ingester.go 的 default 分支 → PlainTextChunker
	}
}

func (c *MCPKnowledgeConnector) Watch(ctx context.Context) (<-chan types.ChangeEvent, error) {
	if !c.SyncConfig().SupportsWatch {
		return nil, apperr.New(apperr.CodeUnimplemented, "watch not supported")
	}
	return nil, apperr.New(apperr.CodeUnimplemented, "mcp knowledge connector watch not fully implemented")
}
