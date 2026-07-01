package native

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// ToolSandbox 向进程内沙箱注册工具函数的最小接口（consumer-side 定义）。
// 实现由 internal/sandbox.InProcessSandbox 提供。
type ToolSandbox interface {
	Register(toolName string, fn sandbox.InProcessFn)
}

// ToolMetaRegistry 向工具目录注册工具元数据的最小接口（consumer-side 定义）。
// 实现由 internal/tool.InMemoryToolRegistry 提供。
type ToolMetaRegistry interface {
	Register(tool types.Tool) error
}

// RegisterExtensionTools 注册原生的 L2 扩展工具。
// 工具元数据从 builtin/<name>/tool.yaml + schema.json 文件加载。
// knowledgeSearcher 为 nil 时跳过 knowledge_search 注册（FeatureDeepRAG 未启用时的降级路径）。
func RegisterExtensionTools(
	sbx ToolSandbox,
	toolReg ToolMetaRegistry,
	extRepo protocol.ExtensionRepository,
	marketplaceClient *marketplace.MCPMarketplaceClient,
	installMgr *marketplace.Manager,
	hitlGateway protocol.HITL,
	outboxWriter protocol.OutboxWriter,
	cognitive CognitiveSearcher,
	embedFn EmbedFn,
	knowledgeSearcher KnowledgeSearcher,
) error {
	defs := []struct {
		name string
		fn   sandbox.InProcessFn
	}{
		{"search_extension", MakeExtensionSearchFn(extRepo, marketplaceClient, cognitive, embedFn)},
		{"install_extension", MakeExtensionInstallFn(extRepo, marketplaceClient, installMgr, hitlGateway, outboxWriter)},
	}

	// knowledge_search：knowledgeSearcher 非 nil 时注册（nil=FeatureDeepRAG 禁用或内存不足降级）
	if knowledgeSearcher != nil {
		defs = append(defs, struct {
			name string
			fn   sandbox.InProcessFn
		}{"knowledge_search", MakeKnowledgeSearchFn(knowledgeSearcher)})
	}

	for _, d := range defs {
		meta, err := LoadExtensionToolMeta(d.name)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("extension_tools: load meta %q", d.name), err)
		}
		sbx.Register(meta.Name, d.fn)
		if err := toolReg.Register(meta); err != nil {
			return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("extension_tools: register %q", d.name), err)
		}
	}

	return nil
}

// knowledgeSearchArgs 是 knowledge_search 工具的入参。
type knowledgeSearchArgs struct {
	Query    string `json:"query"`
	TopK     int    `json:"top_k"`
	DocScope string `json:"doc_scope"`
}

// RegisterExtensionTool 注册单个扩展工具（metadata + fn）到 sandbox 和 toolReg。
// 工具元数据从 embedded builtin/<name>/tool.yaml + schema.json 加载。
// 用于在主注册之后补注需要延迟依赖的工具（如 knowledge_search 依赖 knowledgeBase）。
func RegisterExtensionTool(
	sbx ToolSandbox,
	toolReg ToolMetaRegistry,
	name string,
	fn sandbox.InProcessFn,
) error {
	meta, err := LoadExtensionToolMeta(name)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("extension_tools: load meta %q", name), err)
	}
	sbx.Register(meta.Name, fn)
	if err := toolReg.Register(meta); err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("extension_tools: register %q", name), err)
	}
	return nil
}

// MakeKnowledgeSearchFn 返回 knowledge_search 工具的执行函数。
// 将 KnowledgeSearcher.SearchJSON 包装为 InProcessFn，对齐 builtin 工具调用约定。
func MakeKnowledgeSearchFn(kb KnowledgeSearcher) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args knowledgeSearchArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "knowledge_search: invalid args", err)
		}
		if args.Query == "" {
			return nil, apperr.New(apperr.CodeInternal, "knowledge_search: query is required")
		}
		topK := args.TopK
		if topK <= 0 {
			topK = 5
		}
		if topK > 20 {
			topK = 20
		}
		return kb.SearchJSON(ctx, args.Query, topK, args.DocScope)
	}
}
