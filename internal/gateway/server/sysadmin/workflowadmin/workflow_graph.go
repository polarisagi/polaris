// workflow_graph.go：把 workflow 步骤列表转换为 StateGraphExecutor（编排模式10）
// 可执行的 protocol.WorkflowGraphSpec（2026-07-12 workflow DAG 并行 + 失败重试接入）。
//
// 架构文档: docs/arch/M08-Multi-Agent-Orchestrator.md §3-quinquies
// 决策记录: docs/arch/decisions/ADR-0041-state-graph-orchestration.md
package workflowadmin

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// workflowStepCapabilityType 是所有 workflow 步骤任务在 Blackboard 上统一使用的
// task.Type（复用作 WorkflowNodeSpec.CapabilityType），确保 RunStepWorkerLoop 能
// 精确认领本包投递的任务，不与其他能力类型的任务混淆。步骤自身的 CapabilityType
// 字段（DB capability_type 列）当前仅作元数据保留（对应尚未实现的多能力路由 Saga
// 设计，见 029_workflows.sql 头部注释），不参与图路由——本次接入范围是重试+DAG
// 并行，不是多能力 Agent 路由，避免臆测实现未被要求的能力。
const workflowStepCapabilityType = "workflow_step"

// stepIntentEnvelope 编码进 WorkflowNodeSpec.IntentTemplate 的最小上下文。
// StateGraphExecutor 投递任务时仅知道 node.ID（=step ID）与 IntentTemplate 字符串，
// RunStepWorkerLoop 需要额外的 workflow_id/run_id 才能定位 workflow 配置与写回
// 执行历史（workflow_runs.step_outputs/current_step）。
type stepIntentEnvelope struct {
	WorkflowID string `json:"workflow_id"`
	RunID      string `json:"run_id"`
}

// validateStepRetryCompensation 校验 MaxRetries>0 与 CompensationTool 非空互斥，
// 提前于 HTTP 层拒绝，避免深入 StateGraphExecutor 校验阶段（Execute 调用时）才报错
// ——同一约束见 protocol.WorkflowNodeSpec.MaxVisits 注释（Saga 逆序补偿语义在多次
// 执行节点上未定义）。
func validateStepRetryCompensation(steps []workflowStep) error {
	for _, st := range steps {
		if st.MaxRetries > 0 && st.CompensationTool != "" {
			return apperr.New(apperr.CodeInvalidInput,
				fmt.Sprintf("step %s: max_retries>0 与 compensation_tool 不可同时配置（补偿逆序语义在多次执行节点上未定义）", stepLabel(st)))
		}
	}
	return nil
}

func stepLabel(st workflowStep) string {
	if st.Name != "" {
		return st.Name
	}
	return st.ID
}

// buildGraphSpec 将 workflow 步骤列表转换为 protocol.WorkflowGraphSpec：
//
//   - wfType != "dag"（即默认 "chain"）：忽略 DependsOn，按 Seq 合成顺序链
//     （与既有 chain 语义逐字节等价，仅无条件边，无自环，行为向后兼容）。
//   - wfType == "dag"：如实按 DependsOn 构造边——非空表达真实前驱（StateGraphExecutor
//     的 AND-Join 记账保证多前驱等待全部完成才触发一次）；空表达真正的并行入口
//     （不会被隐式合成为"依赖上一个 Seq"）。DependsOn 的每个元素是**0-based Seq
//     索引的字符串形式**（如 "0"/"2"），而非步骤 DB ID——原因：HandleCreateWorkflow/
//     HandleUpdateWorkflow 每次保存都会为所有步骤重新生成 ID（workflow_steps 整表
//     先删后插，见 store/repo/repo_workflow.go），ID 在两次保存之间不稳定，无法
//     作为前端可持久引用的依赖锚点；Seq 索引在同一次保存内是稳定、用户可见的
//     位置引用（前端"依赖步骤 2"这类勾选 UI 天然以索引呈现）。非法索引（越界/自
//     引用/非数字）静默跳过，不中止整个构图（fail-closed，不做用户输入格式的
//     硬校验中止）。
//   - MaxRetries>0：附加自环条件边（status==error 时重试），MaxVisits=1+MaxRetries。
//   - IsEntry：无外部（非自环）前驱即入口，与是否附带重试自环无关——自环边会让
//     该节点入度恒 > 0，仅靠入度分析无法识别为入口，必须显式标记
//     （见 protocol.WorkflowNodeSpec.IsEntry 注释）。
func buildGraphSpec(wfType, workflowID, runID string, steps []workflowStep) protocol.WorkflowGraphSpec {
	envelopeJSON, _ := json.Marshal(stepIntentEnvelope{WorkflowID: workflowID, RunID: runID})
	envelope := string(envelopeJSON)

	idByIndex := make([]string, len(steps))
	for i, st := range steps {
		idByIndex[i] = st.ID
	}

	// resolveDeps 把 workflowStep.DependsOn 的 Seq 索引字符串解析为实际前驱步骤 ID。
	resolveDeps := func(selfIdx int, rawDeps []string) []string {
		resolved := make([]string, 0, len(rawDeps))
		for _, raw := range rawDeps {
			idx, err := strconv.Atoi(raw)
			if err != nil || idx < 0 || idx >= len(steps) || idx == selfIdx {
				continue
			}
			resolved = append(resolved, idByIndex[idx])
		}
		return resolved
	}

	spec := protocol.WorkflowGraphSpec{
		Nodes: make([]protocol.WorkflowNodeSpec, 0, len(steps)),
		Edges: make([]protocol.WorkflowEdgeSpec, 0, len(steps)),
	}

	for i, st := range steps {
		var deps []string
		if wfType != "dag" {
			// chain 模式：完全忽略 DependsOn，按 Seq 合成顺序链。
			if i > 0 {
				deps = []string{idByIndex[i-1]}
			}
		} else {
			deps = resolveDeps(i, st.DependsOn)
		}

		node := protocol.WorkflowNodeSpec{
			ID:             st.ID,
			CapabilityType: workflowStepCapabilityType,
			IntentTemplate: envelope,
			IsEntry:        len(deps) == 0,
		}
		if st.MaxRetries > 0 {
			node.MaxVisits = 1 + st.MaxRetries
		}
		spec.Nodes = append(spec.Nodes, node)

		for _, dep := range deps {
			spec.Edges = append(spec.Edges, protocol.WorkflowEdgeSpec{From: dep, To: st.ID})
		}
		if st.MaxRetries > 0 {
			spec.Edges = append(spec.Edges, protocol.WorkflowEdgeSpec{
				From: st.ID, To: st.ID,
				Condition: &protocol.EdgeCondition{Field: "status", Op: protocol.CondEquals, Value: "error"},
			})
		}
	}

	return spec
}
