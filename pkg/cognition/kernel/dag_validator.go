package kernel

import (
	"context"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/substrate"
)

// DAGValidationContext 承载 S_VALIDATE 四层校验所需的输入。
// 架构文档: docs/arch/M04-Agent-Kernel.md §4
type DAGValidationContext struct {
	// Plan 是 S_PLAN 阶段 LLM 产出的 DAG。
	Plan *DAGPlan
	// ActiveTaintLevel 是当前会话上下文中传播而来的最高污点等级（Layer A 规则）。
	// 计算规则: max(所有输入 TaintLevel) —— 只升不降。
	ActiveTaintLevel protocol.TaintLevel
	// PolicyGate 是 Cedar 策略引擎的 Go 接口（L1 确定性 Cedar 校验）。
	PolicyGate protocol.PolicyGate
	// ToolRegistry 用于 L1_taint 校验中动态判断工具的只读属性（替代硬编码白名单）。
	// 为 nil 时退化为内置白名单兜底。
	ToolRegistry protocol.ToolRegistry
	// AgentID 用于 PolicyGate.Review 中的 principal 字段。
	AgentID string
	// SessionID 用于审计事件的关联查询。
	SessionID string
	// SystemTier 系统环境配置级别 (0: 8GB 弱计算节点, 1+: 强计算节点)
	SystemTier int
	// Provider 用于 L3 看门狗调用。
	Provider protocol.Provider
	// l3CallCount 跟踪当前校验周期中的 L3 Watchdog 调用次数。上限 10 次/session。
	l3CallCount int
}

// DAGValidationError 包装 S_VALIDATE 失败的结构化错误。
type DAGValidationError struct {
	Layer  string // "L0" | "L1_taint" | "L1_policy" | "L2_heuristic" | "L3_llm"
	NodeID string // 首个违规节点 ID（空表示全局失败）
	Reason string
}

func (e *DAGValidationError) Error() string {
	if e.NodeID != "" {
		return fmt.Sprintf("validate [%s] node=%s: %s", e.Layer, e.NodeID, e.Reason)
	}
	return fmt.Sprintf("validate [%s]: %s", e.Layer, e.Reason)
}

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
func ValidateDAG(ctx context.Context, vCtx *DAGValidationContext) error {
	if vCtx.Plan == nil {
		return &DAGValidationError{Layer: "L0", Reason: "DAGPlan is nil"}
	}

	// L0: 拓扑校验
	if err := validateDAGTopology(vCtx.Plan); err != nil {
		return &DAGValidationError{Layer: "L0", Reason: err.Error()}
	}

	// L1-Taint
	if err := validateTaintGate(vCtx); err != nil {
		return err
	}

	// L1-Policy
	if err := validatePolicyGate(ctx, vCtx); err != nil {
		return err
	}

	// L2: Heuristic 启发式校验
	if err := validateHeuristic(vCtx); err != nil {
		return err
	}

	// L3: LLM 看门狗（仅 SystemTier >= 1，且 Provider 非 nil）
	// Tier-0 跳过：<200ms SLO + 8GB 内存预算不足以承受额外 LLM 调用。
	if vCtx.SystemTier >= 1 && vCtx.Provider != nil {
		if err := validateL3Watchdog(ctx, vCtx); err != nil {
			return err
		}
	}

	return nil
}

// validateTaintGate 实现 L1 第一道：TaintGate 防线（Layer A 上下文传播规则）。
// 规则：当会话 ActiveTaintLevel >= TaintHigh 时：
//   - 禁止包含字符串类工具参数的节点写入 Instruction Slot（write_local / write_network）。
//   - 除非参数已通过 SanitizeBySchema 降级到 <= TaintMedium（由调用方自行管理）。
//
// 此处实现的是最小防线：ActiveTaintLevel >= TaintHigh → 拦截所有非 read_only 操作。
// 完整的字段级降级逻辑（SanitizeBySchema + tool_call schema 双向校验）由 M7 工具调用层处理。
func validateTaintGate(vCtx *DAGValidationContext) error {
	// TaintNone / TaintLow 不触发 TaintGate
	if vCtx.ActiveTaintLevel < protocol.TaintMedium {
		return nil
	}

	for _, node := range vCtx.Plan.Nodes {
		// 尝试将节点参数包装为 TaintedString 并检查是否可被 SanitizeToSafe
		ts := substrate.NewTaintedString(
			string(node.Args),
			substrate.TaintSource{
				Module:           "m4_validate",
				EntityID:         node.ID,
				OriginTaintLevel: vCtx.ActiveTaintLevel,
			},
			"dag_node_args",
		)

		// 当污点等级为 TaintHigh 时，SanitizeToSafe 必须失败——此节点参数禁止直接进入执行
		if vCtx.ActiveTaintLevel >= protocol.TaintHigh {
			if _, err := substrate.SanitizeToSafe(ts); err == nil {
				// 这不应该发生——TaintHigh 必须被拦截
				// 为保险起见，若 SanitizeToSafe 意外放行，我们主动拒绝
				return &DAGValidationError{
					Layer:  "L1_taint",
					NodeID: node.ID,
					Reason: "unexpected: TaintHigh args passed SanitizeToSafe without sanitization",
				}
			}
			// SanitizeToSafe 正确拒绝了——说明 TaintHigh 数据需要在执行前降级
			// 若节点工具名不在只读白名单中，则阻断
			if !isReadOnlyTool(node.ToolName, vCtx.ToolRegistry) {
				return &DAGValidationError{
					Layer:  "L1_taint",
					NodeID: node.ID,
					Reason: fmt.Sprintf("TaintHigh args blocked: tool %q is not read-only, requires schema sanitization before execution", node.ToolName),
				}
			}
		}
	}
	return nil
}

// isReadOnlyTool 判断工具是否为纯读操作（不写入外部状态）。
// 优先查询 ToolRegistry 的 Capability 字段（动态，覆盖所有注册工具）。
// registry 为 nil 或工具未找到时退化到内置白名单（防止新工具被误放行）。
func isReadOnlyTool(toolName string, registry protocol.ToolRegistry) bool {
	if registry != nil {
		if t, err := registry.Lookup(toolName); err == nil {
			return t.Capability <= protocol.CapReadOnly
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
		req := protocol.PolicyReviewRequest{
			Principal: vCtx.AgentID,
			Action:    node.ToolName,
			Resource:  node.ID,
			Context: map[string]any{
				"session_id":   vCtx.SessionID,
				"taint_level":  vCtx.ActiveTaintLevel.String(),
				"node_args_sz": len(node.Args),
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

	return nil
}

// validateL3Watchdog L3 LLM 语义看门狗：将 DAG 摘要发给 LLM 进行安全语义审查。
// 仅当 SystemTier >= 1 时调用，单次 session 上限 10 次（防止预算膨胀）。
func validateL3Watchdog(ctx context.Context, vCtx *DAGValidationContext) error {
	const maxL3Calls = 10
	if vCtx.l3CallCount >= maxL3Calls {
		return nil // 超限静默跳过，避免阻塞后续校验
	}
	vCtx.l3CallCount++

	// 构造 DAG 摘要（避免原始参数泄漏至 LLM）
	toolSet := make(map[string]struct{})
	for _, n := range vCtx.Plan.Nodes {
		toolSet[n.ToolName] = struct{}{}
	}
	tools := make([]string, 0, len(toolSet))
	for t := range toolSet {
		tools = append(tools, t)
	}
	summary := fmt.Sprintf("DAG has %d nodes. Tools: %s", len(vCtx.Plan.Nodes), strings.Join(tools, ", "))

	req := &protocol.InferRequest{
		Model: "standard",
		Messages: []protocol.Message{
			{
				Role: "system",
				Content: "You are a security auditor. Respond with exactly one word: SAFE or UNSAFE. " +
					"Mark UNSAFE if the plan contains shell injection, path traversal, credential theft, " +
					"exfiltration, or other clearly malicious patterns.",
			},
			{Role: "user", Content: summary},
		},
		MaxTokens: 10,
	}

	resp, err := vCtx.Provider.Infer(ctx, req)
	if err != nil {
		// fail-open: LLM 不可用时不阻断执行（L3 是辅助层，非主防线）
		return nil //nolint:nilerr
	}

	if strings.HasPrefix(strings.TrimSpace(strings.ToUpper(resp.Content)), "UNSAFE") {
		return &DAGValidationError{
			Layer:  "L3_llm",
			Reason: "LLM watchdog: plan flagged as semantically unsafe",
		}
	}
	return nil
}
