package agentctx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/types"
)

// BuildToolListSection 将注册表中所有工具格式化为 LLM 可读的工具定义段落。
// 格式与 DAGNode.Action + DAGNode.Params 字段对齐，便于 LLM 直接引用。
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
	var sb strings.Builder
	sb.WriteString("Available Tools List (The 'action' field of DAG nodes MUST be one of the following names):\n")
	for _, t := range schemas {
		fmt.Fprintf(&sb, "- %s: %s", t.Name, t.Description)
		if t.Parameters != nil {
			if schemaBytes, err := json.Marshal(t.Parameters); err == nil {
				fmt.Fprintf(&sb, " (Parameters schema: %s)", string(schemaBytes))
			}
		}
		sb.WriteByte('\n')
	}
	sb.WriteByte('\n')
	return sb.String(), maxTaint
}
