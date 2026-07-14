package dag

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

// 本文件承载 validateTaintGate（validator.go）的字段级降级逻辑（M11 §2.5，
// 2026-07-14 补齐；R7 文件行数治理拆分自 validator.go）：
//   - SanitizeBySchema  —— 工具 InputSchema 严格约束时的结构化数据降级。
//   - SanitizeByUserReview —— HITL 人工复核豁免（复用 ExemptionVault）。

// validateNodeTaint 对单个 DAG 节点执行降级尝试 + 分级拦截判定。
func validateNodeTaint(vCtx *DAGValidationContext, node protocol.ExecNode) error {
	level := attemptSchemaDowngrade(vCtx, node, vCtx.ActiveTaintLevel)
	if level >= types.TaintHigh && level != types.TaintUserReviewed {
		level = attemptUserReviewDowngrade(vCtx, node, level)
	}

	switch {
	case level == types.TaintUserReviewed:
		// 人工已复核（ExemptionVault 内容哈希校验），放行——凭证来自 HE-2 可验证执行
		// 的哈希匹配，非声明式豁免。
		return nil
	case level >= types.TaintHigh:
		// TaintHigh：尝试 SanitizeToSafe；若意外通过则主动拒绝（安全逻辑保险）
		ts := taint.NewTaintedString(
			string(node.Args),
			taint.TaintSource{
				Module:           "m4_validate",
				EntityID:         node.ID,
				OriginTaintLevel: level,
			},
			"dag_node_args",
		)
		if _, err := taint.SanitizeToSafe(ts); err == nil {
			// TaintHigh 数据不应通过 SanitizeToSafe——视为安全逻辑错误
			return &DAGValidationError{
				Layer:  "L1_taint",
				NodeID: node.ID,
				Reason: "unexpected: TaintHigh args passed SanitizeToSafe without sanitization",
			}
		}
		// SanitizeToSafe 正确拒绝——检查工具是否只读；非只读则阻断
		if !isReadOnlyTool(node.ToolName, vCtx.ToolExecutor) {
			return &DAGValidationError{
				Layer:  "L1_taint",
				NodeID: node.ID,
				Reason: fmt.Sprintf("TaintHigh args blocked: tool %q is not read-only, requires schema sanitization before execution", node.ToolName),
			}
		}
	case level >= types.TaintMedium:
		// TaintMedium：仅拦截 write_network（外发请求）；read_only / write_local 允许通过
		// 依据：M04 §3 Layer A——中等可信度数据不应驱动网络外发，但本地操作可接受
		if isWriteNetworkTool(node.ToolName, vCtx.ToolExecutor) {
			return &DAGValidationError{
				Layer:  "L1_taint",
				NodeID: node.ID,
				Reason: fmt.Sprintf("TaintMedium args blocked: tool %q performs network write, requires sanitization to TaintLow first", node.ToolName),
			}
		}
	}
	return nil
}

// attemptSchemaDowngrade 尝试 M11 §2.5 SanitizeBySchema 降级：仅当工具声明的
// InputSchema 对所有字符串叶子字段都有 format/pattern/enum/const 约束时生效
// （hasStrictSchema），裸 {"type":"string"} 一律拒绝降级（fail-closed）。
// level < TaintMedium 或工具未注册时原样返回，不做无意义降级尝试。
func attemptSchemaDowngrade(vCtx *DAGValidationContext, node protocol.ExecNode, level types.TaintLevel) types.TaintLevel {
	if level < types.TaintMedium || vCtx.ToolExecutor == nil {
		return level
	}
	tool, err := vCtx.ToolExecutor.Lookup(node.ToolName)
	if err != nil {
		return level
	}
	ts := taint.NewTaintedString(
		string(node.Args),
		taint.TaintSource{Module: "m4_validate", EntityID: node.ID, OriginTaintLevel: level},
		"dag_node_args",
	)
	downgraded, sanErr := taint.SanitizeBySchema(ts, hasStrictSchema(tool))
	if sanErr != nil {
		return level
	}
	if downgraded.Source.OriginTaintLevel < level {
		slog.Info("s_validate: taint downgraded via SanitizeBySchema (M11 §2.5)",
			"node_id", node.ID, "tool", node.ToolName,
			"from", level.String(), "to", downgraded.Source.OriginTaintLevel.String())
	}
	return downgraded.Source.OriginTaintLevel
}

// attemptUserReviewDowngrade 尝试 M11 §2.5 SanitizeByUserReview 降级：
// vCtx.ReviewChecker 为 nil（未注入）时原样返回，不影响既有拦截行为。
func attemptUserReviewDowngrade(vCtx *DAGValidationContext, node protocol.ExecNode, level types.TaintLevel) types.TaintLevel {
	if vCtx.ReviewChecker == nil || !vCtx.ReviewChecker.IsReviewed(vCtx.AgentID, node.Args) {
		return level
	}
	ts := taint.NewTaintedString(
		string(node.Args),
		taint.TaintSource{Module: "m4_validate", EntityID: node.ID, OriginTaintLevel: level},
		"dag_node_args",
	)
	reviewed := taint.SanitizeByUserReview(ts, "hitl:"+vCtx.AgentID)
	slog.Info("s_validate: taint downgraded via SanitizeByUserReview (M11 §2.5)",
		"node_id", node.ID, "tool", node.ToolName, "agent_id", vCtx.AgentID)
	return reviewed.Source.OriginTaintLevel
}

// hasStrictSchema 判断工具声明的 InputSchema 是否对所有字符串字段都施加了
// format/pattern/enum/const 约束（M11 §2.5 SanitizeBySchema 降级前置条件）。
// InputSchema 类型为 any（builtin 工具通常是 map[string]any，MCP 工具可能是
// json.RawMessage/[]byte），先归一化再递归校验；无法识别的类型 fail-closed 返回 false。
func hasStrictSchema(tool types.Tool) bool {
	root := normalizeSchemaMap(tool.InputSchema)
	if root == nil {
		return false
	}
	return schemaNodeIsStrict(root)
}

// normalizeSchemaMap 将 any 类型的 InputSchema 归一化为 map[string]any。
func normalizeSchemaMap(v any) map[string]any {
	switch s := v.(type) {
	case map[string]any:
		return s
	case json.RawMessage:
		var m map[string]any
		if json.Unmarshal(s, &m) == nil {
			return m
		}
	case []byte:
		var m map[string]any
		if json.Unmarshal(s, &m) == nil {
			return m
		}
	}
	return nil
}

// schemaNodeIsStrict 递归校验 JSON Schema 节点。规则（M11 §2.5）：
//   - 含 properties 的节点（object 根/嵌套 object）：所有子节点必须递归通过，
//     任一深层子节点为无约束裸 string → 整个父结构不可整体降级。
//   - string 类型：必须声明 format/pattern/enum/const 中至少一项。
//   - object 类型但无 properties（自由 additionalProperties）：视为无约束，拒绝。
//   - array 类型：递归校验 items；无 items 声明视为不可信。
//   - number/integer/boolean：不构成注入载体，视为已满足约束。
//   - 其余（未知/缺失 type 且无 properties）：fail-closed 拒绝。
func schemaNodeIsStrict(node map[string]any) bool {
	if props, ok := node["properties"].(map[string]any); ok {
		for _, v := range props {
			child, ok := v.(map[string]any)
			if !ok {
				return false
			}
			if !schemaNodeIsStrict(child) {
				return false
			}
		}
		return true
	}

	typ, _ := node["type"].(string)
	switch typ {
	case "string":
		_, hasFormat := node["format"]
		_, hasPattern := node["pattern"]
		_, hasEnum := node["enum"]
		_, hasConst := node["const"]
		return hasFormat || hasPattern || hasEnum || hasConst
	case "array":
		items, ok := node["items"].(map[string]any)
		if !ok {
			return false
		}
		return schemaNodeIsStrict(items)
	case "number", "integer", "boolean":
		return true
	default:
		// "object" 无 properties（自由 additionalProperties）/ 未知或缺失 type：
		// 均视为无法证明约束存在，fail-closed。
		return false
	}
}
