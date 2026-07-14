package dag

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

// DAGValidationContext / DAGValidationError 权威定义已上移至
// internal/protocol/dag_validation.go（2026-07-12 随 internal/execute 模块化
// 迁出：agent 包不再直接 import 本包，通过 agent/provider.go 声明的 DAGValidator
// 消费端接口消费，字段/方法与本包不再重复维护，此处仅保留别名保证向后兼容）。
type DAGValidationContext = protocol.DAGValidationContext
type DAGValidationError = protocol.DAGValidationError

// ValidateDAG 是 S_VALIDATE 阶段的核心入口，串行执行多道防线。
//
//	L0 (<1ms): 拓扑校验（节点数熔断 + DFS 环检测 + 深度熔断 + 孤立节点）
//	L1-Taint  (<1ms): TaintGate —— 禁止 TaintHigh 参数进入 Instruction Slot
//	L1-Policy (<1ms): PolicyGate —— Cedar deny-by-default，Forbid 规则无条件拦截
//	L2 (<5ms): 启发式检查 —— 并发规模、受保护路径黑名单等
//	L3 (~200ms): LLM 看门狗 —— 仅对 SystemTier >= 1 生效且动作涉及时触发语义检查
//
// 返回 nil 表示全部通过，可推进至 S_EXECUTE。
// 任意层失败返回 *DAGValidationError，调用方应推送 TriggerValidateFail。
func ValidateDAG(ctx context.Context, vCtx *DAGValidationContext) (err error) {
	// 补齐 R3-HE1 可观测优先要求：S_VALIDATE 是安全校验的最后一道防线，
	// 失败必须留痕，否则排障时无法区分"从未进入校验"和"校验主动拒绝"。
	// 不新增 OTel span：internal/agent 包目前没有已建立的 span 埋点惯例，
	// 强行引入会造成孤立的埋点风格,后续排障应从调用方 runValidateDAG 的
	// AgentID/SessionID 日志字段关联,而不是在此处新建一套追踪体系。
	defer func() {
		if err != nil {
			var dagErr *DAGValidationError
			layer, nodeID := "unknown", ""
			if errors.As(err, &dagErr) {
				layer, nodeID = dagErr.Layer, dagErr.NodeID
			}
			slog.Error("s_validate: DAG 校验失败",
				"layer", layer,
				"node_id", nodeID,
				"agent_id", vCtx.AgentID,
				"session_id", vCtx.SessionID,
				"err", err,
			)
		}
	}()

	if vCtx.Plan == nil {
		return &DAGValidationError{Layer: "L0", Reason: "DAGPlan is nil"}
	}

	// L0: 拓扑校验
	if err := validateDAGTopology(vCtx.Plan); err != nil {
		return &DAGValidationError{Layer: "L0", Reason: err.Error()}
	}

	// L1-Taint
	if err := validateTaintGate(vCtx); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "ValidateDAG", err)
	}

	// L1-Policy
	if err := validatePolicyGate(ctx, vCtx); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "ValidateDAG", err)
	}

	// L2: Heuristic 启发式校验
	if err := validateHeuristic(vCtx); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "ValidateDAG", err)
	}

	// L3: LLM 看门狗（仅 SystemTier >= 1，且 Provider 非 nil）
	// Tier-0 跳过：<200ms SLO + 8GB 内存预算不足以承受额外 LLM 调用。
	if vCtx.SystemTier >= 1 && vCtx.Provider != nil {
		// NOTE: L3 validation has been moved to agent_execute.go:runValidateDAG()
		return nil
	}

	return nil
}

// validateTaintGate 实现 L1 第一道：TaintGate 防线（Layer A 上下文传播规则）。
//
// 两档防护：
//   - TaintMedium: 禁止向 write_network 工具传递未降级参数（防止外部数据驱动外发请求）。
//     read_only / write_local 工具允许通过（本地写操作在 TaintMedium 下是可接受的）。
//   - TaintHigh:   拦截所有非 read_only 工具（SanitizeToSafe 必须先失败才表明数据未降级）。
//     意外放行（SanitizeToSafe 返回 nil）视为安全逻辑错误，主动拒绝。
//
// 字段级降级（2026-07-14 补齐，M11 §2.5）：拦截前对每个节点独立尝试两条降级路径
// （均不改变"仍处高位时"的原有拦截行为，只新增放行分支）：
//  1. SanitizeBySchema —— 工具声明的 InputSchema 对所有字符串字段有 format/pattern/
//     enum/const 约束时，node.Args 视为结构化数据而非自由文本注入载体，允许降一级
//     （硬顶 TaintMedium）。
//  2. SanitizeByUserReview —— vCtx.ReviewChecker（复用 ExemptionVault）持有对该
//     node.Args 内容哈希匹配的 HITL 已批准豁免时，标记为 TaintUserReviewed 放行。
func validateTaintGate(vCtx *DAGValidationContext) error {
	// TaintNone / TaintLow 不触发 TaintGate
	if vCtx.ActiveTaintLevel < types.TaintMedium {
		return nil
	}

	for _, node := range vCtx.Plan.Nodes {
		if err := validateNodeTaint(vCtx, node); err != nil {
			return err
		}
	}
	return nil
}

// validateNodeTaint / attemptSchemaDowngrade / attemptUserReviewDowngrade /
// hasStrictSchema / normalizeSchemaMap / schemaNodeIsStrict 见 taint_downgrade.go
// （R7 文件行数治理拆分：本文件专注四层校验管线骨架，字段级降级逻辑独立成文件）。

// isWriteNetworkTool 判断工具是否会触发网络外发（CapWriteNetwork 或以上）。
// 优先查询 ToolRegistry；未注册或 registry 为 nil 时使用内置黑名单（fail-closed）。
func isWriteNetworkTool(toolName string, registry protocol.AgentToolExecutor) bool {
	if registry != nil {
		if t, err := registry.Lookup(toolName); err == nil {
			return t.Capability >= types.CapWriteNetwork
		}
	}
	// 内置黑名单兜底（未知工具默认视为有网络副作用，fail-closed）
	switch toolName {
	case "read_file", "list_dir", "write_file", "get_datetime", "diff_text", "csv_parse",
		"str_replace_editor", "multi_edit", "glob", "grep", "notebook_read", "notebook_edit",
		"todo_read", "todo_write", "git_diff", "template_render", "sys_probe",
		"search_web", "fetch_url": // 纯读网络工具，不向外写入，与 isReadOnlyTool 保持一致
		return false
	}
	return true // 未知工具默认视为网络写，fail-closed
}

// isReadOnlyTool 判断工具是否为纯读操作（不写入外部状态）。
// 优先查询 ToolRegistry 的 Capability 字段（动态，覆盖所有注册工具）。
// registry 为 nil 或工具未找到时退化到内置白名单（防止新工具被误放行）。
func isReadOnlyTool(toolName string, registry protocol.AgentToolExecutor) bool {
	if registry != nil {
		if t, err := registry.Lookup(toolName); err == nil {
			return t.Capability <= types.CapReadOnly
		}
	}
	// 内置白名单兜底（仅对已知工具适用，未知工具默认 fail-closed）
	switch toolName {
	case "read_file", "list_dir", "search_web", "fetch_url":
		return true
	}
	return false
}

// validatePolicyGate 实现 L1 第二道：Cedar PolicyGate 防线（deny-by-default）。
// 逐节点调用 PolicyGate.Review，任一节点被 Forbid → 整体 DAG 拒绝。
// fail-closed: PolicyGate 调用超时或出错 → 拒绝。
func validatePolicyGate(ctx context.Context, vCtx *DAGValidationContext) error {
	if vCtx.PolicyGate == nil {
		// fail-closed: 无策略引擎 → 拒绝所有操作
		return &DAGValidationError{
			Layer:  "L1_policy",
			Reason: "PolicyGate is nil (fail-closed)",
		}
	}

	for _, node := range vCtx.Plan.Nodes {
		req := types.PolicyReviewRequest{
			Principal: vCtx.AgentID,
			Action:    node.ToolName,
			Resource:  node.ID,
			Context: map[string]any{
				"session_id":         vCtx.SessionID,
				"taint_level":        vCtx.ActiveTaintLevel.String(),
				"node_args_sz":       len(node.Args),
				"monthly_spend_usd":  vCtx.MonthlySpendUSD,
				"monthly_budget_usd": vCtx.MonthlyBudgetUSD,
			},
		}

		result, err := vCtx.PolicyGate.Review(ctx, req)
		if err != nil {
			// fail-closed: 评估异常 → 拒绝
			return &DAGValidationError{
				Layer:  "L1_policy",
				NodeID: node.ID,
				Reason: fmt.Sprintf("PolicyGate.Review error (fail-closed): %v", err),
			}
		}
		if !result.Allowed {
			return &DAGValidationError{
				Layer:  "L1_policy",
				NodeID: node.ID,
				Reason: fmt.Sprintf("PolicyGate denied: %s", result.Reason),
			}
		}
	}

	return nil
}

// validateHeuristic 实现 L2: Heuristic 启发式校验。
// 架构要求: 批量规模(>100) → 受保护路径(`/etc/`,`/sys/`,`~/.ssh/`→拒绝) → 资源预估。
func validateHeuristic(vCtx *DAGValidationContext) error {
	// 1. 并发/批量规模检查
	if len(vCtx.Plan.Nodes) > 100 {
		return &DAGValidationError{
			Layer:  "L2_heuristic",
			Reason: fmt.Sprintf("DAG scale exceeded limit: %d nodes > 100", len(vCtx.Plan.Nodes)),
		}
	}

	// 2. 危险路径黑名单检查 (仅针对文件读写工具)
	forbiddenPaths := []string{"/etc/", "/sys/", "/boot/", "~/.ssh/"}
	for _, node := range vCtx.Plan.Nodes {
		if node.ToolName == "read_file" || node.ToolName == "write_file" || node.ToolName == "bash" {
			argsStr := string(node.Args)
			for _, path := range forbiddenPaths {
				if strings.Contains(argsStr, path) {
					return &DAGValidationError{
						Layer:  "L2_heuristic",
						NodeID: node.ID,
						Reason: fmt.Sprintf("heuristic block: accessed protected path %q", path),
					}
				}
			}
		}
	}

	// 3. 不可逆副作用节点（write_network / write_local）必须声明 Compensation
	irreversibleTypes := map[string]bool{
		"write_network": true,
		"write_local":   true,
	}
	for _, node := range vCtx.Plan.Nodes {
		if irreversibleTypes[node.ToolName] && node.Compensation == nil {
			return &DAGValidationError{
				Layer:  "L2_heuristic",
				NodeID: node.ID,
				Reason: fmt.Sprintf("heuristic block: node %q (tool=%q) has side effects but missing Compensation declaration (Saga safety)", node.ID, node.ToolName),
			}
		}
	}

	return nil
}
