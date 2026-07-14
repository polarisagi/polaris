package agent

import (
	"github.com/polarisagi/polaris/internal/observability/metrics"

	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	agentctx "github.com/polarisagi/polaris/internal/agent/context"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func canvasResultSummary(res *types.ToolResult, err error) string {
	if err != nil {
		return err.Error()
	}
	if res == nil {
		return ""
	}
	if !res.Success {
		if res.Error != "" {
			return res.Error
		}
		return "failed"
	}
	return string(res.Output)
}

//nolint:gocyclo // MVP intercept logic
func (a *Agent) interceptComputerUse(ctx context.Context, toolName string, args []byte) error {
	if toolName != "computer_use" && toolName != "browser_use" {
		return nil
	}

	// Cedar 策略预检（R3）：deny-by-default，先于 HITL 审批。
	// LAM engine 为 nil 时跳过（保持与无 LAM 场景兼容）。
	if a.lamEngine != nil {
		if pErr := a.lamEngine.CheckPolicy(ctx, args); pErr != nil {
			return pErr //nolint:wrapcheck
		}
	}

	mode := "auto_review"
	if a.sCtx.Preferences != nil {
		if v, ok := a.sCtx.Preferences["computer_use_mode"]; ok && v != "" {
			mode = v
		}
	}

	isDangerous := false
	switch toolName {
	case "computer_use":
		var actionReq struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(args, &actionReq)
		if actionReq.Action == "key" || actionReq.Action == "type" || actionReq.Action == "left_click" || actionReq.Action == "right_click" || actionReq.Action == "double_click" || actionReq.Action == "left_click_drag" {
			isDangerous = true
		}
	case "browser_use":
		var actionReq struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(args, &actionReq)
		if actionReq.Action == "click" || actionReq.Action == "type" || actionReq.Action == "key" {
			isDangerous = true
		}
	}

	needHITL := false
	if mode == "default" {
		needHITL = true
	} else if mode == "auto_review" && isDangerous {
		needHITL = true
	}

	if needHITL && a.hitl != nil {
		prompt := types.HITLPrompt{
			ID:             uuid.New().String(),
			AgentID:        a.sCtx.AgentID,
			CheckpointType: types.CheckpointDeviceControlReview,
			PromptText:     fmt.Sprintf("Agent requests to execute %s with args: %s\nMode: %s", toolName, string(args), mode),
			Options: []types.HITLOption{
				{Key: "approve", Label: "Approve"},
				{Key: "deny", Label: "Deny"},
			},
			DeadlineNs: time.Now().Add(5 * time.Minute).UnixNano(),
			// PermissionMode 供 resolveTimeoutAction 区分：仅 full_access 下超时
			// 才允许兜底为 auto_approve，与"设置 → 设备操控"承诺的语义一致。
			PermissionMode: types.PermissionMode(mode),
		}
		respHITL, hitlErr := a.hitl.Prompt(ctx, prompt)
		if hitlErr != nil || respHITL == nil || respHITL.OptionKey != "approve" {
			if hitlErr != nil {
				return apperr.Wrap(apperr.CodeForbidden, "HITL gateway denied computer use action", hitlErr)
			}
			return apperr.New(apperr.CodeForbidden, "HITL gateway denied computer use action")
		}
	}
	return nil
}

// aggregateDAGResults 将多节点执行结果聚合为统一 JSON 格式。
// 单节点直接返回 output；多节点序列化为 {"results":[{id,output},...]}.
func aggregateDAGResults(results []protocol.NodeResult) []byte {
	if len(results) == 0 {
		return []byte("{}")
	}
	if len(results) == 1 {
		if results[0].Err != nil {
			return []byte(`{"error":"` + results[0].Err.Error() + `"}`)
		}
		if len(results[0].Output) == 0 {
			if len(results[0].ImageParts) > 0 {
				return []byte("[Success (Image Attached)]")
			}
			return []byte("[Success (Empty Output)]")
		}
		return results[0].Output
	}

	// 多节点：构建聚合结构
	buf := make([]byte, 0, 256+len(results)*64)
	buf = append(buf, `{"results":[`...)
	for i, r := range results {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, `{"id":"`...)
		buf = append(buf, r.NodeID...)
		buf = append(buf, `","output":`...)
		if r.Err != nil {
			buf = append(buf, `{"error":"`...)
			buf = append(buf, r.Err.Error()...)
			buf = append(buf, `"}`...)
		} else if len(r.Output) > 0 {
			buf = append(buf, r.Output...)
		} else if len(r.ImageParts) > 0 {
			buf = append(buf, `"[Success (Image Attached)]"`...)
		} else {
			buf = append(buf, `"[Success (Empty Output)]"`...)
		}
		buf = append(buf, '}')
	}
	buf = append(buf, "]}"...)
	return buf
}

// maxExecResultBytes 注入 LLM 的工具执行结果最大字节数（≈ 2000 token × 4 bytes/token）。
const maxExecResultBytes = 8000

// truncateExecResult 截断过长的执行结果，超限部分落盘并返回 log_ref 占位符。
// 落盘路径：~/.polarisagi/polaris/logs/exec_results/<logID>.txt
// LLM 收到：原文（≤8KB）或 <log_ref id="<logID>" bytes="<N>" /> 提示符（>8KB）
func truncateExecResult(sessionID string, raw []byte) []byte {
	if len(raw) <= maxExecResultBytes {
		return raw
	}

	logID := fmt.Sprintf("%s-%d", sessionID, time.Now().UnixNano())
	logDir := filepath.Join(os.ExpandEnv("$HOME"), ".polarisagi", "polaris", "logs", "exec_results")
	// 创建目录（best-effort，失败不阻断）
	if err := os.MkdirAll(logDir, 0700); err == nil {
		logPath := filepath.Join(logDir, logID+".txt")
		_ = os.WriteFile(logPath, raw, 0600)
	}

	// 截取前 512 字节作为内联预览，其余引用 log_ref
	preview := raw[:512]
	ref := fmt.Sprintf(
		"<log_ref id=%q bytes=%d />\n[Preview]\n%s\n[...truncated, see log]",
		logID, len(raw), preview,
	)
	return []byte(ref)
}

// maxNodeTaintLevel 计算 protocol.DAGPlan 中所有节点的最高污点等级。
// 实现 ADR-0007 PropagateTaint 语义：output = max(inputs)，只升不降。
// plan 为 nil 或无节点时返回 TaintNone（validateTaintGate 自动跳过）。
func maxNodeTaintLevel(plan *protocol.DAGPlan) types.TaintLevel {
	if plan == nil {
		return types.TaintNone
	}
	var max types.TaintLevel
	for _, node := range plan.Nodes {
		if node.TaintLevel > max {
			max = node.TaintLevel
		}
	}
	return max
}

// extractTaskType 从任务目标字符串提取规范化任务类型键。
// 与 swarm.ExtractTaskType 保持一致，避免 L1 到 L2 的依赖。
func extractTaskType(goal string) string {
	words := strings.Fields(strings.ToLower(goal))
	if len(words) == 0 {
		return "unknown"
	}
	if len(words) > 3 {
		words = words[:3]
	}
	return strings.Join(words, "_")
}

func (a *Agent) injectMemoryToMsgs(ctx context.Context, msgs []types.Message) []types.Message {
	if a.assembler == nil || a.sCtx.TaskModel == nil {
		return msgs
	}

	maxT := a.sCtx.RawIntentTS.Source.OriginTaintLevel
	if maxT == types.TaintNone {
		maxT = types.TaintHigh
	}
	req := agentctx.AssembleRequest{
		Query:                 a.sCtx.TaskModel.Goal,
		SessionKey:            a.sCtx.SessionID,
		MaxTokens:             2000,
		MaxTaint:              maxT,
		IncludeKnowledge:      true,
		SurpriseHint:          metrics.GlobalSurpriseIndex().Current(),
		SurpriseHintThreshold: a.Config.SurpriseHintThreshold,
	}

	ac, err := a.assembler.Assemble(ctx, req)
	if err != nil || len(ac.Items) == 0 {
		return msgs
	}

	var sb strings.Builder
	sb.WriteString("Relevant Context:\n")
	for _, item := range ac.Items {
		fmt.Fprintf(&sb, "- [%s] %s\n", item.Source, item.Content)
	}

	return append([]types.Message{{Role: "system", Content: sb.String()}}, msgs...)
}

func (a *Agent) writeEpisodicWithExtract(ctx context.Context, ev types.Event) {
	if a.memory == nil {
		return
	}
	if err := a.memory.AppendEpisodicEvent(ctx, ev, types.TaintNone); err != nil {
		a.handleMemoryPersistenceFailure(ctx, err, ev)
		return
	}

	if a.outboxWriter == nil {
		return
	}

	switch string(ev.Type) {
	case "task_perceived", "plan_generated", "reflection_completed", "execution_completed", string(types.EventActionPending), string(types.EventActionDone):
		sessionID := ev.TaskID
		if sessionID == "" && a.sCtx != nil {
			sessionID = a.sCtx.SessionID
		}
		outboxEv, _ := protocol.NewOutboxEvent(protocol.TopicEpisodicExtract, "episodic_extract", map[string]any{
			"session_id": sessionID,
			"event_type": string(ev.Type),
			"content":    string(ev.Payload),
		}, ev.ID+":extract")
		_ = a.outboxWriter.Write(ctx, outboxEv)
	}
}

// handleMemoryPersistenceFailure 处理 Episodic 写入失败（GD-13-003 FSM 熔断）。
//
// HE-6（State-in-DB）：Episodic 事件是 Agent 状态外化的核心路径之一，Reflect/Replan
// 等后续决策均依赖历史事件序列。持久化失败若被静默吞掉（旧实现 `_ = a.memory.
// AppendEpisodicEvent(...)`），会导致 Agent 在残缺状态上继续决策而不自知——违反
// HE-1（可观测优先）与 HE-6。
//
// 只对存储层不可用（CodeStorageUnavailable）熔断；序列化失败等其他 CodeInternal
// 错误只记录日志不熔断，避免偶发的单条事件构造失败误杀整条执行链路。
//
// 熔断机制复用 agent_execute_dag.go capability_gap 先例，不新增 FSM 转换规则、
// 不新增挂起态类型（HE-3 可组合原语 + ADR-0042 先例）：
//  1. 设置 sCtx.SuspendReason，供 HITL/运维侧观测挂起原因；
//  2. 经 outbox 异步投递 m9_storage_degraded 事件，供运维告警/自动恢复 Worker 消费；
//  3. 调用 a.asyncIntent(TriggerInterruptReceived) —— 该 trigger 在
//     fsm/state_machine.go Dispatch() 中作为状态无关的全局处理（见该文件 S_INTERRUPT
//     通用处理分支），可从任意非终态直接进入 S_INTERRUPT，无需在 transitions.go
//     注册表中新增专用转换规则。
func (a *Agent) handleMemoryPersistenceFailure(ctx context.Context, err error, ev types.Event) {
	if !isMemoryPersistenceFailure(err) {
		slog.Error("writeEpisodicWithExtract: episodic 写入失败（非存储层故障，不熔断）",
			"error", err, "event_type", ev.Type, "task_id", ev.TaskID)
		return
	}

	slog.Error("writeEpisodicWithExtract: 检测到存储层持久化失败，触发 FSM 熔断",
		"error", err, "event_type", ev.Type, "task_id", ev.TaskID)

	if a.sCtx != nil {
		a.sCtx.SuspendReason = "memory_persistence_failure"
	}

	if sqlRepo, ok := a.taskRepo.(protocol.SQLQuerier); ok && sqlRepo != nil {
		payloadBytes, _ := json.Marshal(map[string]string{
			"error":      err.Error(),
			"event_type": string(ev.Type),
			"task_id":    ev.TaskID,
		})
		_, _ = sqlRepo.ExecContext(ctx, `
			INSERT INTO outbox (created_at, target_engine, operation, scope, payload, idempotency_key, status)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, time.Now().UnixMilli(), "m9_storage_degraded", "upsert", "memory_persistence_failure",
			payloadBytes, uuid.New().String(), "pending")
	}

	a.asyncIntent(types.TriggerInterruptReceived)
}

// isMemoryPersistenceFailure 判断错误是否为存储层不可用（而非序列化等其他内部错误）。
func isMemoryPersistenceFailure(err error) bool {
	return apperr.IsCode(err, apperr.CodeStorageUnavailable)
}

// withTaskScopeCtx 把当前会话标识注入 ctx，供 tokenizeMessagesForLLM 写令牌、
// internal/tool/tool.go ExecuteTool 与 execute/dag/executor.go DAGExecutor.Execute 还原令牌时
// 使用同一 taskID 命名空间（M11 §5.1 PII OpaqueToken 任务级隔离）。
//
// 必须使用 a.sCtx.SessionID，不能用 a.sCtx.TaskID——二者是不同字段：TaskID 是
// 当前认领的 Blackboard task_id，由 Worker 在每次 Run() 前通过 SetTaskID() 注入，
// 会随会话内认领的任务变化；SessionID 贯穿整个 Agent 会话生命周期不变，且是仓库
// 既有惯例里传给 protocol.CtxTaskIDKey 的值（见 fsm/state_machine.go §422-423
// 注释、agent_execute_dag.go 里 a.dagRunner.Run(ctx, plan, toolExecFn, nil, a.sCtx.SessionID, a.sCtx.AgentID)
// 调用点）。写入令牌与还原令牌若使用不同字段，会导致同一次调用链前后用不上同一个
// taskID 命名空间，隔离和清理都会失效。
//
// SessionID 为空（例如脱离 Agent 生命周期的单元测试直接构造 Agent{} 场景）时不做
// 任何注入，保留调用方传入的 ctx 原样，交由 tokenizeMessagesForLLM 内部继续用空
// taskID 兜底（等价于 legacy Tokenize/Resolve/Restore 路径）。
func (a *Agent) withTaskScopeCtx(ctx context.Context) context.Context {
	if a.sCtx != nil && a.sCtx.SessionID != "" {
		ctx = context.WithValue(ctx, protocol.CtxTaskIDKey{}, a.sCtx.SessionID)
	}
	// anomalyFilter 恒非 nil（NewAgent 默认构造，见 agent.go），随任务域 ctx 一并
	// 注入，供 internal/tool/tool.go checkAnomaly 读取（ADR-0051 关联接线）。
	if a.anomalyFilter != nil {
		ctx = context.WithValue(ctx, protocol.CtxAnomalyFilterKey{}, a.anomalyFilter)
	}
	return ctx
}

// tokenizeMessagesForLLM 在消息送入 LLM Provider 前，对每条 message.Content 做 PII 令牌化。
// piiDetector/tokenVault 任一为 nil 时直接跳过（不阻塞主流程，Tier0 无 Presidio 场景下的降级行为）。
func (a *Agent) tokenizeMessagesForLLM(ctx context.Context, messages []types.Message) ([]types.Message, error) {
	if a.piiDetector == nil || a.tokenVault == nil {
		return messages, nil
	}
	out := make([]types.Message, len(messages))
	for i, m := range messages {
		out[i] = m
		if m.Content == "" {
			continue
		}
		// RedactWithMode 会内部从 ctx 提取 CtxTaskIDKey 并调用 TokenizeForTask
		tokenized, n, err := a.piiDetector.RedactWithMode(ctx, m.Content, "tokenize", nil, a.tokenVault)
		if err != nil {
			slog.Error("agent: PII tokenization failed, fail-closed", "err", err)
			// 选择 fail-closed 策略：如果 PII 提取失败，截断流程，防止明文 PII 流入 LLM。
			return nil, apperr.Wrap(apperr.CodeInternal, "tokenizeMessagesForLLM", err)
		}
		if n > 0 {
			out[i].Content = tokenized
		}
	}
	return out, nil
}
