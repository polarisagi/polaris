package agent

import (
	"github.com/polarisagi/polaris/internal/security/token"

	"github.com/polarisagi/polaris/internal/observability/metrics"

	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/agent/dag"
	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// budgetWarnPct / fsm.BudgetCriticalPct：Token 预算分级阈值（百分比，整数）。
// 50% → 告警日志；75% → BudgetPressure 标记（S_PLAN 收紧 DAG）；100% → INFERENCE_OOM 硬失败。
const (
	budgetWarnPct = 50
)

func (a *Agent) executeEffect(ctx context.Context, effect protocol.Effect) error { //nolint:gocyclo
	var nextState types.State
	var err error

	if effect.IsLLMFill() { //nolint:nestif
		llmEff, ok := effect.(protocol.LLMFillEffect)
		if !ok {
			return apperr.New(apperr.CodeInternal, "invalid LLMFillEffect type")
		}

		// 1. Budget Control: 分级预算检查
		if a.sCtx.TokenBudget > 0 {
			used := a.sCtx.TokensUsed
			budget := a.sCtx.TokenBudget

			switch {
			case used > budget:
				// 硬断路：INFERENCE_OOM，任务失败。
				// M3 在下一轮 TokenBurnRate 检测时自驱触发 KillSwitch CheckAndAct。
				a.sm.ForceState(types.AgentStateFailed)
				return apperr.New(apperr.CodeInternal,
					fmt.Sprintf("INFERENCE_OOM: token budget exceeded (%d > %d)", used, budget))

			case used*100/budget >= fsm.BudgetCriticalPct:
				// 软阈值 75%：注入预算压力标记，S_PLAN 生成 DAG 时收紧规模。
				// 已处于 S_EXECUTE 时该标记无效（DAG 已生成，继续执行到底）。
				if !a.sCtx.BudgetPressure {
					a.sCtx.BudgetPressure = true
					slog.Warn("kernel: budget critical threshold reached, plan scope will be reduced",
						"agent_id", a.ID,
						"tokens_used", used,
						"budget", budget,
						"pct", used*100/budget)
				}

			case used*100/budget >= budgetWarnPct:
				// 软阈值 50%：写一次日志，不改变行为。
				if !a.sCtx.BudgetWarned {
					a.sCtx.BudgetWarned = true
					slog.Info("kernel: budget warning threshold reached",
						"agent_id", a.ID,
						"tokens_used", used,
						"budget", budget,
						"pct", used*100/budget)
				}
			}
		}

		if a.provider == nil {
			return apperr.New(apperr.CodeInternal, "agent missing provider for LLMFillEffect")
		}

		// 1.5 KillSwitch Check
		stage := metrics.GlobalKillswitchStage.Load()
		switch stage {
		case int32(security.KillFullStop):
			a.sm.ForceState(types.AgentStateFailed)
			return apperr.New(apperr.CodeInternal, "killswitch: stage=FullStop, refusing new inference")
		case int32(security.KillPause):
			return apperr.New(apperr.CodeInternal, "killswitch: stage=Pause, suspending task")
		case int32(security.KillThrottle):
			// Stage 1 降级：收紧步骤预算至 3，标记禁用网络写工具（M03 §5 ThrottlePolicy）。
			if a.sCtx.MaxStepsLimit == 0 || a.sCtx.MaxStepsLimit > 3 {
				a.sCtx.MaxStepsLimit = 3
			}
			a.sCtx.ThrottleNoNetwork = true
		}

		var resp *types.ProviderResponse
		var inferErr error

		// 2. System 1/2 Routing & World Model Inference Skip
		// 如果在 S_PERCEIVE 阶段，且 SurpriseIndex 很低 (<0.3)，走 FastPath 跳过 LLM
		// System I 快思考路径：SurpriseIndex < 0.3 时直接旁路 LLM，对应三轨推理引擎的"降维"轨道。
		if a.sm.Current() == types.AgentStatePerceive {
			// FastPath (M09 Logic Collapse): 跳过 S_PERCEIVE LLM 推理，产生合成感知结果。
			// 不操作 DAGModel/ExecuteResult——外部注入的 DAGModel 保持不变。
			// 具体能力识别将在后续通过 intent 向量检索实现（ADR-M09）。
			// 注意: SurpriseIndex == 0 表示"未计算"（默认值），不触发 FastPath。
			if a.sCtx.SurpriseIndex > 0 && a.sCtx.SurpriseIndex < 0.3 {

				fastResult := `{"Goal":` + a.sCtx.RawIntentTS.MarshalJSONString() + `,"Complexity":0.1}`
				nextState, err = llmEff.OnSuccess(protocol.StateContext{}, []byte(fastResult))
				if a.memory != nil {
					go func(intentJSON string) {
						a.writeEpisodicWithExtract(context.Background(), types.Event{
							ID:        uuid.New().String(),
							Type:      types.EventIntent,
							Status:    types.StatusDone,
							TaskID:    a.sCtx.SessionID,
							AgentID:   a.sCtx.AgentID,
							Payload:   []byte(`{"intent":` + intentJSON + `}`),
							CreatedAt: time.Now(),
						})
					}(a.sCtx.RawIntentTS.MarshalJSONString())
				}
				goto HANDLE_MEM
			}
		}

		// S_PLAN 阶段
		if a.sm.Current() == types.AgentStatePlan {
			// ── BlindZone 检查（V8-S4，GEMINI_PATCH_ROUND23_V8）─────────────────
			// 若该任务类型生产出现≥5次但 MEMF 零记录，强制升级到 System2 路由。
			// 原理：系统对该类任务缺乏失败记忆闭环，快速路由的"信心"来源不明。
			if a.blindZoneDetector != nil && a.sCtx.TaskModel != nil {
				taskType := extractTaskType(a.sCtx.TaskModel.Goal)
				a.blindZoneDetector.RecordProduction(taskType)
				if a.blindZoneDetector.IsBlindZone(ctx, taskType) {
					// 强制落入 System2 路由区间（SurpriseIndex ≥ 0.6）
					// 不覆盖已经更高的 SurpriseIndex
					if a.sCtx.SurpriseIndex < 0.65 {
						a.sCtx.SurpriseIndex = 0.65
					}
					a.sCtx.BlindZoneHITLRequired = true
					metrics.GlobalBlindZoneRoutingTotal.Add(1)
				}
			}
			// ── BlindZone 检查结束 ────────────────────────────────────────────────

			if a.worldModel != nil && a.sCtx.TaskModel != nil {
				// 注入上下文给 WorldModel 进行知识接地评估
				// 这里使用 TaskModel.Goal 作为 task，并将 SysEnvSnapshot 等作为 contextText
				ok, gap := a.worldModel.AssessGrounding(a.ctx, a.sCtx.TaskModel.Goal, a.sCtx.SysEnvSnapshot)
				if !ok && gap != "" {
					a.sCtx.GroundingGap = gap
				} else {
					a.sCtx.GroundingGap = ""
				}
			}

			if a.sCtx.SurpriseIndex > 0 && a.sCtx.SurpriseIndex < 0.3 {
				// FastPath 路径：S_PERCEIVE 已坍缩，直接旁路 LLM 规划。
				// DAGModel 为 nil 时 runExecuteDAG 直接推进 ExecuteDone，
				// 为非 nil 时执行已生成的 DAG（高置信路径）。
				// SurpriseIndex == 0 表示"未计算"，不触发。
				nextState = "S_PLAN_DONE"
				err = nil
				if a.memory != nil {
					go func(intentJSON string) {
						a.writeEpisodicWithExtract(context.Background(), types.Event{
							ID:        uuid.New().String(),
							Type:      types.EventIntent,
							Status:    types.StatusDone,
							TaskID:    a.sCtx.SessionID,
							AgentID:   a.sCtx.AgentID,
							Payload:   []byte(`{"intent":` + intentJSON + `,"path":"fast_plan"}`),
							CreatedAt: time.Now(),
						})
					}(a.sCtx.RawIntentTS.MarshalJSONString())
				}
				goto HANDLE_MEM
			}

			// PRM 多候选路径：并发生成 N 个方案，打分选最优。
			// System II 慢思考路径（中级）：PRM Judge Agent 对 N 个 DAG 候选方案打分，
			// 选出最优方案执行。对应三轨推理引擎的"升维-PRM轨道"。
			if a.prm != nil &&
				a.sCtx.TaskModel != nil &&
				a.prm.ShouldActivate(a.sCtx.TaskModel.Complexity) {

				n := a.prm.MaxCandidates()
				baseMessages := llmEff.PromptFn(a.toProtocolCtx())
				baseMessages = a.injectMemoryToMsgs(ctx, baseMessages)

				type candidateResult struct {
					plan   *types.DAGModel
					tokens int
				}
				candidateCh := make(chan candidateResult, n)

				for range n {
					go func() {
						cResp, cErr := a.provider.Infer(ctx, baseMessages, types.WithModel(llmEff.ModelPool), types.WithThinkingMode(llmEff.ThinkingMode))
						if cErr != nil {
							candidateCh <- candidateResult{}
							return
						}
						var plan types.DAGModel
						if jsonErr := json.Unmarshal([]byte(cResp.Content), &plan); jsonErr != nil {
							candidateCh <- candidateResult{}
							return
						}
						candidateCh <- candidateResult{
							plan:   &plan,
							tokens: cResp.Usage.InputTokens + cResp.Usage.OutputTokens,
						}
					}()
				}

				var candidates []*types.DAGModel
				for range n {
					cr := <-candidateCh
					a.sCtx.TokensUsed += cr.tokens
					if cr.plan != nil {
						candidates = append(candidates, cr.plan)
					}
				}

				if len(candidates) > 0 {
					best, selectErr := a.prm.SelectBest(ctx, a.sCtx.TaskModel.Goal, a.sCtx.TaskModel.Complexity, candidates)
					if selectErr != nil || best == nil {
						best = candidates[0]
					}
					bestJSON, _ := json.Marshal(best)
					// 构造合成响应，保证 HANDLE_MEM 处的记忆写入正常触发
					resp = &types.ProviderResponse{Content: string(bestJSON)}
					nextState, err = llmEff.OnSuccess(a.toProtocolCtx(), bestJSON)
					goto HANDLE_MEM
				}
				// 所有候选均失败时降级到单次 Infer
			}
		}

		{
			reqMsgs := llmEff.PromptFn(a.toProtocolCtx())
			reqMsgs = a.injectMemoryToMsgs(ctx, reqMsgs)
			resp, inferErr = a.provider.Infer(ctx, reqMsgs, types.WithModel(llmEff.ModelPool), types.WithThinkingMode(llmEff.ThinkingMode))
			if inferErr != nil {
				if errors.Is(inferErr, llm.ErrAllProvidersFailed) {
					a.sCtx.ProviderSuspendCount++
					if a.sCtx.ProviderSuspendCount >= 5 && a.hitl != nil {
						hitlResp, hitlErr := a.hitl.Prompt(ctx, types.HITLPrompt{
							ID:             fmt.Sprintf("hitl_%d", time.Now().UnixNano()),
							CheckpointType: "provider_exhausted",
							PromptText:     "All providers have failed 5 times consecutively. Approve to reset suspension counter.",
							DeadlineNs:     time.Now().Add(5 * time.Minute).UnixNano(),
						})
						if hitlErr == nil && hitlResp != nil && hitlResp.Approved {
							a.sCtx.ProviderSuspendCount = 0
						} else {
							return apperr.New(apperr.CodeInternal, "provider_exhausted hitl denied")
						}
					} else {
						// 写 DB：标记任务为 suspended，供 recovery.go 恢复扫描
						if sqlRepo, ok := a.taskRepo.(protocol.SQLQuerier); ok && sqlRepo != nil {
							updateSQL := `UPDATE tasks SET status='suspended', suspend_reason='provider_exhausted', provider_suspended_count=?, updated_at=? WHERE task_id=?`
							_, dbErr := sqlRepo.ExecContext(ctx, updateSQL, a.sCtx.ProviderSuspendCount, time.Now().UTC(), a.sCtx.TaskID)
							if dbErr != nil {
								slog.Warn("agent: failed to write suspended status", "task_id", a.sCtx.TaskID, "err", dbErr)
							}
						}
						// 持久化 PII 快照（M04 §8 SuspendSnapshot 约定）
						if a.piiVault != nil && a.sCtx.TaskID != "" {
							piiFields := map[string]string{}
							a.sCtx.RawIntentTS.AppendToMap(piiFields, "raw_intent")
							if a.sCtx.SessionID != "" {
								piiFields["session_id"] = a.sCtx.SessionID
							}
							if len(piiFields) > 0 {
								if snapErr := a.piiVault.Snapshot(ctx, a.sCtx.TaskID, piiFields); snapErr != nil {
									slog.Warn("agent: pii snapshot failed on suspend", "task_id", a.sCtx.TaskID, "err", snapErr)
								}
							}
						}
					}
					// 继续原有的 Suspended 状态机转移逻辑（不 return，让 FSM 处理后续）
				}
				nextState, err = llmEff.OnFailure(a.toProtocolCtx(), inferErr)
			} else {
				// 累计分项 Token（Gap-A：分开记录供 Worker 写回 Blackboard）
				a.sCtx.TokensInput += resp.Usage.InputTokens
				a.sCtx.TokensOutput += resp.Usage.OutputTokens
				a.sCtx.TokensCacheRead += resp.Usage.CacheHitTokens
				a.sCtx.TokensUsed += resp.Usage.InputTokens + resp.Usage.OutputTokens
				// 保存 reasoning_content 供下轮消息历史回传（BUG-04 fix）
				if resp.ReasoningContent != "" {
					a.sCtx.LastReasoningContent = resp.ReasoningContent
				}
				nextState, err = llmEff.OnSuccess(a.toProtocolCtx(), []byte(resp.Content))
			}
		}

	HANDLE_MEM:
		// ReplayMode 物理短路：回放时不写副作用，防止双写 EventLog / Outbox。
		if protocol.IsReplaying() {
			return nil
		}
		// 成功完成感知，将用户意图作为事件写入记忆（由于当前缺失 TaskIntent，仅做预留演示）
		if nextState == "S_PERCEIVE_DONE" && a.memory != nil && (resp != nil || a.sCtx.SurpriseIndex < 0.3) {
			var content string
			if resp != nil {
				content = resp.Content
			} else {
				content = `{"Goal":` + a.sCtx.RawIntentTS.MarshalJSONString() + `,"Complexity":0.1}`
			}
			eventID := a.sm.NextEventID(a.sCtx.SessionID, "perceive")
			a.writeEpisodicWithExtract(ctx, types.Event{
				ID:        eventID,
				Type:      "task_perceived",
				Payload:   []byte(content),
				CreatedAt: time.Now(),
			})
			if a.outboxWriter != nil {
				evPayload, _ := json.Marshal(types.Event{
					ID:        eventID,
					Type:      "task_perceived",
					TaskID:    a.sCtx.SessionID,
					Payload:   []byte(content),
					CreatedAt: time.Now(),
				})
				_ = a.outboxWriter.Write(ctx, protocol.OutboxEntry{
					TargetEngine:   "episodic",
					Operation:      "project",
					Payload:        evPayload,
					IdempotencyKey: a.sCtx.SessionID + ":perceive:" + a.sCtx.AgentID,
				})
			}
		}

		// 成功完成计划，写入计划记忆
		if nextState == "S_PLAN_DONE" && a.memory != nil && (resp != nil || a.sCtx.SurpriseIndex < 0.3) {
			// 恢复步骤预算 (ISSUE-08)
			if a.sCtx.InitialMaxStepsLimit > 0 {
				a.sCtx.MaxStepsLimit = a.sCtx.InitialMaxStepsLimit
			}

			var content string
			if resp != nil {
				content = resp.Content
			} else if a.sCtx.DAGModel != nil {
				planBytes, _ := json.Marshal(a.sCtx.DAGModel)
				content = string(planBytes)
			}
			eventID := a.sm.NextEventID(a.sCtx.SessionID, "plan")
			a.writeEpisodicWithExtract(ctx, types.Event{
				ID:        eventID,
				Type:      "plan_generated",
				Payload:   []byte(content),
				CreatedAt: time.Now(),
			})
			if a.outboxWriter != nil {
				evPayload, _ := json.Marshal(types.Event{
					ID:        eventID,
					Type:      "plan_generated",
					TaskID:    a.sCtx.SessionID,
					Payload:   []byte(content),
					CreatedAt: time.Now(),
				})
				_ = a.outboxWriter.Write(ctx, protocol.OutboxEntry{
					TargetEngine:   "episodic",
					Operation:      "project",
					Payload:        evPayload,
					IdempotencyKey: a.sCtx.SessionID + ":plan:" + a.sCtx.AgentID,
				})
			}
		}

		// 成功完成反思，写入反思记忆，并保存 ReasoningState
		if nextState == "S_REFLECT_DONE" && a.memory != nil && (resp != nil || a.sCtx.SurpriseIndex < 0.3) {
			var content string
			if resp != nil {
				content = resp.Content
			}
			// 保存至内存上下文以供跨轮次携带
			a.sCtx.ReasoningState = []byte(content)

			eventID := a.sm.NextEventID(a.sCtx.SessionID, "reflect")
			a.writeEpisodicWithExtract(ctx, types.Event{
				ID:        eventID,
				Type:      "reflection_completed",
				Payload:   []byte(content),
				CreatedAt: time.Now(),
			})
			if a.outboxWriter != nil {
				evPayload, _ := json.Marshal(types.Event{
					ID:        eventID,
					Type:      "reflection_completed",
					TaskID:    a.sCtx.SessionID,
					Payload:   []byte(content),
					CreatedAt: time.Now(),
				})
				_ = a.outboxWriter.Write(ctx, protocol.OutboxEntry{
					TargetEngine:   "episodic",
					Operation:      "project",
					Payload:        evPayload,
					IdempotencyKey: a.sCtx.SessionID + ":reflect:" + a.sCtx.AgentID,
				})
			}
			// 触发 Episodic → Semantic 4 阶段记忆蒸馏（ConsolidationPipeline，M5 §4）
			if a.outboxWriter != nil && a.sCtx.SessionID != "" {
				consolidatePayload, _ := json.Marshal(map[string]string{"session_id": a.sCtx.SessionID})
				_ = a.outboxWriter.Write(ctx, protocol.OutboxEntry{
					Operation:      "memory_consolidate",
					Payload:        consolidatePayload,
					IdempotencyKey: a.sCtx.SessionID + ":consolidate",
				})
			}
		}
	} else {
		detEff, ok := effect.(protocol.DeterministicEffect)
		if !ok {
			return apperr.New(apperr.CodeInternal, "invalid DeterministicEffect type")
		}

		// S_VALIDATE 阶段拦截：调用 Agent 层四层校验（可访问 policyGate 与完整 sCtx）。
		// 此分支由 runValidateDAG 自行通过 SendIntent 推进 FSM（ValidateOk / ValidateFail），
		// 因此直接返回，不走 stateToTriggerMap 路径，避免双重推进。
		if a.sm.Current() == types.AgentStateValidate {
			// FastPath 空执行路径（SurpriseIndex 触发但无 LLM 生成 DAG）：nil DAGModel 直接放行。
			// runValidateDAG 对 nil plan 会触发 L0 拦截，因此在进入前短路。
			if a.sCtx.DAGModel == nil {
				go func() { _ = a.SendIntent(types.TriggerValidateOk) }()
				return nil
			}
			if err := a.runValidateDAG(ctx); err != nil {
				// 业务校验失败会触发 ValidateFail，不应被视为系统级致命错误导致 Run 崩溃退出
				slog.Debug("kernel: validate DAG", "err", err)
			}
			return nil
		}

		// S_EXECUTE 阶段拦截：调用 Agent 层 DAG 执行（可访问 toolRegistry 与完整 sCtx）。
		// 同理，由 runExecuteDAG 自行推进 FSM（ExecuteDone / ExecuteFail）。
		if a.sm.Current() == types.AgentStateExecute {
			// runExecuteDAG 内负责在完成后将结果写入 a.sCtx.ExecuteResult
			err := a.runExecuteDAG(ctx)
			if err == nil && a.memory != nil && len(a.sCtx.ExecuteResult) > 0 {
				eventID := a.sm.NextEventID(a.sCtx.SessionID, "exec")
				a.writeEpisodicWithExtract(ctx, types.Event{
					ID:        eventID,
					Type:      "execution_completed",
					Payload:   a.sCtx.ExecuteResult,
					CreatedAt: time.Now(),
				})
				if a.outboxWriter != nil {
					evPayload, _ := json.Marshal(types.Event{
						ID:        eventID,
						Type:      "execution_completed",
						TaskID:    a.sCtx.SessionID,
						Payload:   a.sCtx.ExecuteResult,
						CreatedAt: time.Now(),
					})
					_ = a.outboxWriter.Write(ctx, protocol.OutboxEntry{
						TargetEngine:   "episodic",
						Operation:      "project",
						Payload:        evPayload,
						IdempotencyKey: a.sCtx.SessionID + ":exec:" + a.sCtx.AgentID,
					})
				}
			}
			// 业务执行失败会触发 ExecuteFail，同样不抛出以免阻断状态机
			return nil
		}

		if detEff.Fn != nil {
			nextState, err = detEff.Fn(ctx, a.toProtocolCtx())
		}
	}

	// 优先判断是否有逻辑状态推进。如果有，说明 FSM 已经接管了这个业务错误，我们不抛出致命异常
	if nextState != "" {
		if trigger, ok := stateToTriggerMap()[nextState]; ok {
			go func() { _ = a.SendIntent(trigger) }()
			return nil
		}
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("unknown next state: %s (err: %v)", nextState, err))
	}

	// 只有当没有状态流转时，才把底层技术错误抛出导致 Agent 终止
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "Agent.executeEffect", err)
	}

	return nil
}

// runValidateDAG 是 Agent 层面的四层校验入口。
// 与 StateMachine.validateDAG 的区别在于：
//   - 能够访问 a.sCtx.DAGModel（LLM 产出的 DAG）
//   - 能够访问 a.policyGate（Cedar 引擎）
//   - 返回结构化的 State 令牌以推进 FSM
func (a *Agent) runValidateDAG(ctx context.Context) error {
	var plan *dag.DAGPlan
	if a.sCtx.DAGModel != nil {
		plan = &dag.DAGPlan{
			Nodes: a.sCtx.DAGModel.Nodes,
			Edges: a.sCtx.DAGModel.Edges,
		}
	}

	vCtx := &dag.DAGValidationContext{
		Plan: plan,
		// ActiveTaintLevel 取 DAGModel 所有节点的最高 TaintLevel（PropagateTaint 语义）。
		// 依据: ADR-0007 自然传播规则——output = max(inputs)，只升不降。
		// 不直接用 RawIntentTS.Level()（固定 TaintHigh）原因：
		//   validateTaintGate 对 TaintHigh 会拦截所有非只读工具；
		//   而节点 TaintLevel 来自 parsePlanOnSuccess 中 pCtx.MaxTaintLevel，
		//   若 LLM 已将摘要降级为 TaintMedium，节点级应反映该降级结果。
		// 若所有节点均为 TaintNone（FastPath/空 DAG），网关自然跳过（< TaintMedium）。
		ActiveTaintLevel: maxNodeTaintLevel(plan),
		PolicyGate:       a.policyGate,
		ToolRegistry:     a.toolRegistry, // 用于 isReadOnlyTool 动态查询工具 Capability
		AgentID:          a.sCtx.AgentID,
		SessionID:        a.sCtx.SessionID,
		SystemTier:       a.Config.SystemTier, // 由 M3 HardwareProbe 探测后通过 AgentConfig.SystemTier 注入
	}

	if err := dag.ValidateDAG(ctx, vCtx); err != nil {
		// 校验失败→ 异步推送 TriggerValidateFail 以面向 FSM 的 S_REPLAN
		go func() { _ = a.SendIntent(types.TriggerValidateFail) }()
		// 返回非致命 error 提示调用方失败原因，但不能让 Run 循环崩溃
		return apperr.Wrap(apperr.CodeInternal, "s_validate failed", err)
	}

	// L3: LLM 看门狗校验 (上提为标准 FSM Effect)
	// 仅对 Tier 1+ 生效
	if vCtx.SystemTier >= 1 && a.provider != nil && vCtx.Plan != nil { //nolint:nestif
		var dangerous []string
		for _, node := range vCtx.Plan.Nodes {
			tool, err := a.toolRegistry.Lookup(node.ToolName)
			if err != nil {
				continue
			}
			// L3 仅针对 RiskPrivileged 节点，非只读但低风险节点已由 L1/L2 覆盖
			if tool.RiskLevel == types.RiskPrivileged {
				dangerous = append(dangerous, fmt.Sprintf("Tool: %s, Args: %s", node.ToolName, string(node.Args)))
			}
		}

		if len(dangerous) > 0 {
			prompt := "Review the following tool executions for security risks:\n" + strings.Join(dangerous, "\n") + "\n\nIf it's dangerous, reply with 'DENY: <reason>'. Otherwise reply 'ALLOW'."

			llmEff := protocol.LLMFillEffect{
				SchemaRef: "l3_watchdog",
				PromptFn: func(pCtx protocol.StateContext) []types.Message {
					return []types.Message{
						{Role: "system", Content: "You are a strict security watchdog."},
						{Role: "user", Content: prompt},
					}
				},
				OnSuccess: func(pCtx protocol.StateContext, content []byte) (types.State, error) {
					if strings.HasPrefix(strings.ToUpper(string(content)), "DENY") {
						go func() { _ = a.SendIntent(types.TriggerValidateFail) }()
						return "S_VALIDATE_FAIL", apperr.New(apperr.CodeForbidden, "LLM Watchdog denied: "+string(content))
					}
					go func() { _ = a.SendIntent(types.TriggerValidateOk) }()
					return "S_VALIDATE_OK", nil
				},
				OnFailure: func(pCtx protocol.StateContext, err error) (types.State, error) {
					// L3 失败时 fail-open——架构设计，非疏漏。
					// 依据: M04 §L3 LLM 看门狗: "LLM 不可用时 fail-open 推进 S_VALIDATE_OK"。
					// L3 是补充信号层：L0/L1/L2 未放行的动作不可因 L3 通过而放行；
					// L3 DENY 推进 ValidateFail，L3 LLM 不可用时不应因此阻断正常业务流。
					// 禁止改为 fail-closed：L3 LLM 故障会导致所有非只读 DAG 永久卡住。
					go func() { _ = a.SendIntent(types.TriggerValidateOk) }()
					return "S_VALIDATE_OK", nil
				},
				MaxRetry:  0, // 看门狗不重试
				ModelPool: "reasoning",
			}

			// 递归执行该 Effect，利用标准流程调用 LLM 并计费
			return a.executeEffect(ctx, llmEff)
		}
	}

	// 校验通过→ 异步推送 TriggerValidateOk
	go func() { _ = a.SendIntent(types.TriggerValidateOk) }()
	return nil
}

// runExecuteDAG 是 Agent 层面的 DAG 执行入口。
// 从 a.sCtx.DAGModel 构建 dag.DAGPlan，通过 dag.DAGExecutor 按拓扑序并发执行工具，
// 结果写入 a.sCtx.ExecuteResult。
// 任意节点失败 → 推送 TriggerExecuteFail（触发 S_ROLLBACK 和 Saga 补偿）。
func (a *Agent) runExecuteDAG(ctx context.Context) error { //nolint:gocyclo
	if a.sCtx.DAGModel == nil {
		// DAGModel 为空时跳过执行（等价于空 DAG），直接推进 ExecuteDone
		go func() { _ = a.SendIntent(types.TriggerExecuteDone) }()
		return nil
	}

	if a.toolRegistry == nil {
		// fail-closed: 无工具注册表时拒绝执行
		go func() { _ = a.SendIntent(types.TriggerExecuteFail) }()
		return apperr.New(apperr.CodeInternal, "runExecuteDAG: toolRegistry is nil (fail-closed)")
	}

	plan := &dag.DAGPlan{
		Nodes: a.sCtx.DAGModel.Nodes,
		Edges: a.sCtx.DAGModel.Edges,
	}

	var callCount atomic.Int32

	// 将 ToolRegistry.ExecuteTool 绑定为 dag.DAGExecutor 的工具执行函数
	toolExecFn := func(ctx context.Context, toolName string, args []byte, taintLevel types.TaintLevel) (*types.ToolResult, error) {
		tokenVal := ctx.Value(protocol.CtxCapabilityToken{})
		if token, ok := tokenVal.(*token.Token); ok && token != nil {
			max := int32(token.Claims.MaxCallsPerTask)
			if max > 0 && callCount.Load() >= max {
				return nil, apperr.New(apperr.CodeForbidden, "capability token: max_calls_per_task exceeded")
			}
			callCount.Add(1)
		}

		if toolName == "spawn_planner" {
			// spawn_planner 特殊处理：不走普通工具执行路径，而是：
			// 1. 发送 InterruptRequest{Action: InterruptResume}（挂起自身，等待 whisperChan）
			// 2. 返回特殊的待定结果，主脑将依靠 PlannerPool 推送的结果恢复
			// 3. 在这里不直接创建 PlannerPool，而是通过专门的回调或外层触发，
			// 或者如果允许的话，在这里异步启动 PlannerPool。
			// (稍后将通过 go func() 启动 PlannerPool)

			// 解析参数
			var argsMap map[string]string
			_ = json.Unmarshal(args, &argsMap)
			goal := argsMap["goal"]
			taskType := argsMap["task_type"]
			if taskType == "" {
				taskType = "general"
			}

			if a.plannerSpawner != nil {
				go a.plannerSpawner(ctx, goal, taskType, a.provider)
			}

			// 发送挂起意图
			go func() { _ = a.SendIntent(types.TriggerInterruptReceived) }()

			return &types.ToolResult{
				Success:   true,
				Suspended: true,
				Output:    []byte("Planner pool spawned, agent suspended waiting for whisper."),
			}, nil
		}

		tool, err := a.toolRegistry.Lookup(toolName)
		isIdempotent := true
		if err == nil {
			for _, se := range tool.SideEffects {
				if se != types.SideNone {
					isIdempotent = false
					break
				}
			}
		}

		// KillThrottle 执行层拦截：禁止携带 network_call 副作用的工具（ADR-0009 §Stage1）。
		if a.sCtx.ThrottleNoNetwork && err == nil {
			for _, se := range tool.SideEffects {
				if se == types.SideNetworkCall {
					return nil, apperr.New(apperr.CodeForbidden,
						fmt.Sprintf("killswitch throttle: tool %q blocked (network_call side-effect)", toolName))
				}
			}
		}

		var pendingEventID string
		// ReplayMode：跳过 2PC 预写，工具副作用已由 dag_executor 层短路。
		if protocol.IsReplaying() {
			goto SKIP_2PC
		}
		// [2PC Phase 1] 检查是否曾意外崩溃，并预写日志
		if !isIdempotent { //nolint:nestif
			query := types.EpisodicQuery{
				SessionID: a.sCtx.SessionID,
			}
			events, err := a.memory.Episodic().Query(ctx, query)
			if err == nil {
				hasPending := false
				hasDone := false
				signature := fmt.Sprintf(`"tool":"%s"`, toolName)
				for _, e := range events {
					if strings.Contains(string((func() *types.Event {
						if e, _ := e.Event.(*types.Event); e != nil {
							return e
						}
						return &types.Event{}
					}()).Payload), signature) {
						if (func() *types.Event {
							if e, _ := e.Event.(*types.Event); e != nil {
								return e
							}
							return &types.Event{}
						}()).Type == types.EventActionPending {
							hasPending = true
						} else if (func() *types.Event {
							if e, _ := e.Event.(*types.Event); e != nil {
								return e
							}
							return &types.Event{}
						}()).Type == types.EventActionDone {
							hasDone = true
						}
					}
				}
				if hasPending && !hasDone {
					// Crashed during execution. 阻断以防止外部副作用重复发生
					return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("double execution prevented: non-idempotent tool %s was interrupted previously", toolName))
				}
				if hasDone {
					// 已成功执行但后续环节崩溃导致重跑
					return &types.ToolResult{
						Success: true,
						Output:  []byte(fmt.Sprintf("tool %s was already executed successfully", toolName)),
					}, nil
				}
			}

			// 写预写日志 Action_Pending
			pendingEventID = uuid.New().String()
			a.writeEpisodicWithExtract(ctx, types.Event{
				ID:        pendingEventID,
				Type:      types.EventActionPending,
				Status:    types.StatusExecuting,
				TaskID:    a.sCtx.SessionID,
				AgentID:   a.sCtx.AgentID,
				Payload:   []byte(fmt.Sprintf(`{"tool":"%s","args":%s}`, toolName, string(args))),
				CreatedAt: time.Now(),
			})
		}
	SKIP_2PC:

		// HITL 拦截逻辑 (Computer Use Confirmations Policy)
		if errHITL := a.interceptComputerUse(ctx, toolName, args); errHITL != nil {
			//nolint:nilerr // ToolExecutor expects error to be reported in ToolResult for LLM to see
			return &types.ToolResult{
				Success: false,
				Error:   errHITL.Error(),
			}, nil
		}

		start := time.Now()
		res, err := a.toolRegistry.ExecuteTool(ctx, toolName, args, taintLevel)
		latencyMs := time.Since(start).Milliseconds()

		// Adaptive Max-Steps: 为每次工具调用打分，低分时收紧步骤预算
		if a.scorer != nil {
			toolOK := err == nil && res != nil && res.Success
			sc := a.scorer.score(stepCtx{
				ToolName:     toolName,
				LatencyMs:    latencyMs,
				TokensUsed:   0, // 工具调用不消耗 token，此维度不惩罚
				SchemaPassed: true,
				ToolResult:   toolOK,
			})
			a.sCtx.MaxStepsLimit = adjustMaxSteps(a.sCtx.MaxStepsLimit, sc)
		}

		// [2PC Phase 2] 执行完成，写入日志闭环
		if !isIdempotent && pendingEventID != "" {
			status := types.StatusDone
			if err != nil || (res != nil && !res.Success) {
				status = types.StatusFailed
			}
			a.writeEpisodicWithExtract(ctx, types.Event{
				ID:        uuid.New().String(),
				Type:      types.EventActionDone,
				Status:    status,
				TaskID:    a.sCtx.SessionID,
				AgentID:   a.sCtx.AgentID,
				Payload:   []byte(fmt.Sprintf(`{"tool":"%s","status":"%s"}`, toolName, status)),
				CreatedAt: time.Now(),
			})
		}

		if err == nil && res != nil && res.Success {
			toolDef, lookupErr := a.toolRegistry.Lookup(toolName)
			if lookupErr == nil && toolDef.UndoFn != "" {
				a.sCtx.SagaLog = append(a.sCtx.SagaLog, types.SagaStep{
					NodeID:   toolName, // executor 不传 NodeID，暂以 toolName 代替
					ToolName: toolName,
					UndoFn:   toolDef.UndoFn,
					Args:     args,
				})
			}
			// Logic Collapse 触发器：记录工具调用成功轨迹（M9 §4 Skill 蒸馏）
			if a.toolCallRecorder != nil {
				a.toolCallRecorder.RecordToolSuccess(ctx, toolName)
			}
		}

		if err != nil {
			return res, apperr.Wrap(apperr.CodeInternal, "Agent.runExecuteDAG", err)
		}
		return res, nil
	}

	executor := dag.NewDAGExecutor(toolExecFn, nil) // leaseRenew 由 M8 注入，MVP 传 nil
	results, err := executor.Execute(ctx, plan, a.sCtx.SessionID, a.sCtx.AgentID)

	if executor.DegradedReplan {
		a.sCtx.DegradedReplan = true
	}

	if err != nil {
		if strings.Contains(err.Error(), "tool not found") {
			a.sCtx.SuspendReason = "capability_gap"

			// 通过 outbox 异步投递 m9_capability_gap 事件，触发 GapFillWorker 进行能力补全
			if sqlRepo, ok := a.taskRepo.(protocol.SQLQuerier); ok && sqlRepo != nil {
				payloadBytes, _ := json.Marshal(map[string]string{"error": err.Error()})
				_, _ = sqlRepo.ExecContext(ctx, `
					INSERT INTO background_tasks (id, agent_id, status, type, args_json, created_at)
					VALUES (?, ?, 'pending', 'prompt_optimization', ?, ?)
				`, "opt_"+a.ID+"_"+time.Now().Format("150405"), a.ID, `{"target_metric": "quality"}`, time.Now().Unix())
				_, _ = sqlRepo.ExecContext(ctx, `
					INSERT INTO outbox (created_at, target_engine, operation, scope, payload, idempotency_key, status)
					VALUES (?, ?, ?, ?, ?, ?, ?)
				`, time.Now().UnixMilli(), "m9_capability_gap", "upsert", "capability_gap", payloadBytes, uuid.New().String(), "pending")
			}

			go func() { _ = a.SendIntent(types.TriggerInterruptReceived) }()
			return nil
		}

		// 执行失败 → 触发 S_ROLLBACK
		go func() { _ = a.SendIntent(types.TriggerExecuteFail) }()
		return apperr.Wrap(apperr.CodeInternal, "runExecuteDAG: DAG execution failed", err)
	}

	// 检查是否有节点挂起
	for _, res := range results {
		if res.Suspended {
			// spawn_planner 等工具已触发中断，无需再发 ExecuteDone
			return nil
		}
	}

	// 聚合所有节点输出为 JSON 数组，反思阶段可获取完整 DAG 执行结果。
	// 单节点时保持向后兼容（直接取 output 字节）；多节点时序列化为 {"results":[...]} 结构。
	raw := aggregateDAGResults(results)
	a.sCtx.ExecuteResult = truncateExecResult(a.sCtx.SessionID, raw)
	go func() { _ = a.SendIntent(types.TriggerExecuteDone) }()
	return nil
}

//nolint:gocyclo // MVP intercept logic
func (a *Agent) interceptComputerUse(ctx context.Context, toolName string, args []byte) error {
	if toolName != "computer_use" && toolName != "browser_use" {
		return nil
	}
	mode := "auto_review"
	if a.sCtx.Preferences != nil {
		if v, ok := a.sCtx.Preferences["computer_use_mode"]; ok && v != "" {
			mode = v
		}
	}

	isDangerous := false
	if toolName == "computer_use" {
		var actionReq struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(args, &actionReq)
		if actionReq.Action == "key" || actionReq.Action == "type" || actionReq.Action == "left_click" || actionReq.Action == "right_click" || actionReq.Action == "double_click" || actionReq.Action == "left_click_drag" {
			isDangerous = true
		}
	} else if toolName == "browser_use" {
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
			CheckpointType: "security_review",
			PromptText:     fmt.Sprintf("Agent requests to execute %s with args: %s\nMode: %s", toolName, string(args), mode),
			Options: []types.HITLOption{
				{Key: "approve", Label: "Approve"},
				{Key: "deny", Label: "Deny"},
			},
			DeadlineNs: time.Now().Add(5 * time.Minute).UnixNano(),
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
func aggregateDAGResults(results []dag.NodeResult) []byte {
	if len(results) == 0 {
		return []byte("{}")
	}
	if len(results) == 1 {
		if results[0].Err != nil {
			return []byte(`{"error":"` + results[0].Err.Error() + `"}`)
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
		} else {
			buf = append(buf, `null`...)
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

// maxNodeTaintLevel 计算 dag.DAGPlan 中所有节点的最高污点等级。
// 实现 ADR-0007 PropagateTaint 语义：output = max(inputs)，只升不降。
// plan 为 nil 或无节点时返回 TaintNone（validateTaintGate 自动跳过）。
func maxNodeTaintLevel(plan *dag.DAGPlan) types.TaintLevel {
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
	if a.memInjector == nil || a.sCtx.TaskModel == nil {
		return msgs
	}
	memCtx, err := a.memInjector.InjectRelevantMemory(ctx, a.sCtx.SessionID, a.sCtx.TaskModel.Goal)
	if err != nil || memCtx == "" {
		return msgs
	}

	return append([]types.Message{{Role: "system", Content: "Relevant Memory Context:\n" + memCtx}}, msgs...)
}

func (a *Agent) writeEpisodicWithExtract(ctx context.Context, ev types.Event) {
	if a.memory == nil {
		return
	}
	_ = a.memory.Episodic().Append(ctx, ev)

	if a.outboxWriter == nil {
		return
	}

	switch string(ev.Type) {
	case "task_perceived", "plan_generated", "reflection_completed", "execution_completed", string(types.EventActionPending), string(types.EventActionDone):
		sessionID := ev.TaskID
		if sessionID == "" && a.sCtx != nil {
			sessionID = a.sCtx.SessionID
		}
		payload, _ := json.Marshal(map[string]any{
			"session_id": sessionID,
			"event_type": string(ev.Type),
			"content":    string(ev.Payload),
		})
		_ = a.outboxWriter.Write(ctx, protocol.OutboxEntry{
			TargetEngine:   "memory",
			Operation:      "episodic_extract",
			Payload:        payload,
			IdempotencyKey: ev.ID + ":extract",
		})
	}
}
