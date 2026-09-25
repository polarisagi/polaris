package agentctx

import (
	"context"
	"strings"

	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/types"
)

// BuildToolListSection 将注册表中的工具名称格式化为 DAGNode.Action 的合法取值清单
// （完整定义走原生 function-calling，见函数体注释）。
//
// ctx 必须携带 protocol.CtxTaskIDKey（由调用方从 sCtx.SessionID 注入），否则
// 懒加载模式下 search_tools 在上一轮激活的工具（CompositeCatalog.ActivateTool）
// 无法在本轮 Schemas() 重建时命中同一激活作用域——见 internal/tool/catalog/composite.go
// 的 Schemas() 与 internal/tool/tool_search.go 的 sessionIDFromCtx，两处必须使用
// 同一个 TaskID 才能让"搜索到的工具在后续轮次真正可调用"这个懒加载协议闭环。
//
// BuildToolListSection 返回值第二项为该次目录中出现过的最高来源污点等级
// （S-02，M11 §3）：
//   - types.ToolBuiltin（系统内置，硬编码路径）→ TaintNone
//   - types.ToolSkill（本地已安装 Skill/Plugin，经签名校验）→ TaintLow
//   - 其余来源（尤其 types.ToolMCP：外部服务器可随时改描述，等同远端可控输入；
//     以及 ToolA2A/ToolLLMGenerated 等未来来源）→ fail-closed 按 TaintHigh 处理
//
// 调用方必须用 WriteExternalCatalog 写入，不得再拼进 TaintNone 的内核指令区。
func BuildToolListSection(ctx context.Context, cata catalog.Catalog) (string, types.TaintLevel) {
	if cata == nil {
		return "", types.TaintNone
	}
	// TrustCommunity 是通常的默认门槛，如果有更高要求可传入不同值
	schemas := cata.Schemas(ctx, types.TrustCommunity)
	if len(schemas) == 0 {
		return "", types.TaintNone
	}
	entries := cata.List(ctx, types.TrustCommunity)
	maxTaint := types.TaintNone
	for _, e := range entries {
		var t types.TaintLevel
		switch e.Source {
		case types.ToolBuiltin:
			t = types.TaintNone
		case types.ToolSkill:
			t = types.TaintLow
		default:
			t = types.TaintHigh
		}
		if t > maxTaint {
			maxTaint = t
		}
	}
	// 只列名称（ADR-0102 决策五，收紧 ADR-0101 决策五的"名称 + 描述"）：S_PLAN 同时经
	// 原生 function-calling 下发完整定义（agent_execute_effect.go WithTools），名称、描述、
	// 参数 schema 都在其中，文本里再写描述仍是重复计费（内置 56 个工具）。JSON-DAG 输出路径只需知道
	// action 的合法取值，参数结构模型可从原生工具定义读取；不支持原生 tools 的
	// Provider 由其适配器自行把 schema 渲染成文本（adapter.renderToolsAsText）。
	var sb strings.Builder
	sb.WriteString("Available tool names (the 'action' field of DAG nodes MUST be one of these; " +
		"descriptions and parameter schemas are provided via the function-calling tool definitions):\n")
	for i, t := range schemas {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(t.Name)
	}
	sb.WriteString("\n\n")
	return sb.String(), maxTaint
}
