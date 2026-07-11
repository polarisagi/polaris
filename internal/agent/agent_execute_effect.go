package agent

import (
	"github.com/polarisagi/polaris/internal/observability/metrics"

	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/agent/fsm"
	"github.com/polarisagi/polaris/internal/agent/schemavalidate"
	"github.com/polarisagi/polaris/internal/llm/safecall"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
	"github.com/polarisagi/polaris/pkg/types"
)

func (a *Agent) executeEffect(ctx context.Context, effect protocol.Effect) error { //nolint:gocyclo
	ctx = a.withTaskScopeCtx(ctx)

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
				// System 1 FastPath：先查技能缓存（M09 Logic Collapse 蒸馏产出）
				// 命中则直接执行 Python 脚本，绕过 LLM；未命中退回合成 JSON 路径。
				// 技能缓存命中路径：GetOrSpawn 验证技能在注册表中存在，SkillExecutor 执行 Python 脚本。
				// 两者均需注入（WithSkillCache + WithSkillExecutor）；任一为 nil 时跳过，退回合成 JSON 路径。
				if a.skillCache != nil && a.skillExecutor != nil && a.sCtx.TaskModel != nil {
					skillID := extractTaskType(a.sCtx.TaskModel.Goal)
					if handle, cacheErr := a.skillCache.GetOrSpawn(ctx, skillID); cacheErr == nil && handle != nil {
						// ProcessHandle 仅作"已确认可用"令牌，实际执行委托给 SkillExecutor，
						// 由 M7 ScriptSkillExecutor 完成脚本加载和沙箱执行。
						runCtx, runCancel := context.WithTimeout(ctx, 200*time.Millisecond)
						output, runErr := a.skillExecutor.ExecuteSkill(runCtx, handle.SkillID, []byte(a.sCtx.RawIntentTS.Content()))
						runCancel() // 立即释放：超时上下文只覆盖 ExecuteSkill 调用，不扩散到后续异步 goroutine
						if runErr == nil && len(output) > 0 {
							scriptResult := string(output)
							nextState, err = llmEff.OnSuccess(protocol.StateContext{}, []byte(scriptResult))
							metrics.GlobalSkillCacheHitTotal.Add(1) // 可观测：缓存命中计数
							if a.memory != nil {
								localIntent := a.sCtx.RawIntentTS.MarshalJSONString()
								localResult := scriptResult
								concurrent.SafeGo(ctx, "agent.episodic_memory_write", func(ctx context.Context) {
									a.writeEpisodicWithExtract(ctx, types.Event{
										ID:        uuid.New().String(),
										Type:      types.EventIntent,
										Status:    types.StatusDone,
										TaskID:    a.sCtx.SessionID,
										AgentID:   a.sCtx.AgentID,
										Payload:   []byte(`{"intent":` + localIntent + `,"skill_result":` + localResult + `}`),
										CreatedAt: time.Now(),
									})
								})
							}
							goto HANDLE_MEM
						}
					}
				}
				// 技能缓存未命中或不可用：退回合成 JSON 路径（现有行为保持不变）
				fastResult := `{"Goal":` + a.sCtx.RawIntentTS.MarshalJSONString() + `,"Complexity":0.1}`
				nextState, err = llmEff.OnSuccess(protocol.StateContext{}, []byte(fastResult))
				if a.memory != nil {
					localIntent := a.sCtx.RawIntentTS.MarshalJSONString()
					concurrent.SafeGo(ctx, "agent.episodic_memory_write", func(ctx context.Context) {
						a.writeEpisodicWithExtract(ctx, types.Event{
							ID:        uuid.New().String(),
							Type:      types.EventIntent,
							Status:    types.StatusDone,
							TaskID:    a.sCtx.SessionID,
							AgentID:   a.sCtx.AgentID,
							Payload:   []byte(`{"intent":` + localIntent + `}`),
							CreatedAt: time.Now(),
						})
					})
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
					localIntent := a.sCtx.RawIntentTS.MarshalJSONString()
					concurrent.SafeGo(ctx, "agent.episodic_memory_write", func(ctx context.Context) {
						a.writeEpisodicWithExtract(ctx, types.Event{
							ID:        uuid.New().String(),
							Type:      types.EventIntent,
							Status:    types.StatusDone,
							TaskID:    a.sCtx.SessionID,
							AgentID:   a.sCtx.AgentID,
							Payload:   []byte(`{"intent":` + localIntent + `,"path":"fast_plan"}`),
							CreatedAt: time.Now(),
						})
					})
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
				baseMessages, err = a.tokenizeMessagesForLLM(ctx, baseMessages)
				if err != nil {
					return apperr.Wrap(apperr.CodeInternal, "agent: failed to tokenize messages for PRM candidates, fail-closed", err)
				}

				type candidateResult struct {
					plan   *types.DAGModel
					tokens int
				}
				candidateCh := make(chan candidateResult, n)

				for range n {
					concurrent.SafeGo(ctx, "agent.prm_candidate_infer", func(ctx context.Context) {
						cResp, cErr := safecall.Infer(ctx, a.provider, baseMessages,
							types.WithModel(llmEff.ModelPool),
							types.WithThinkingMode(llmEff.ThinkingMode),
							types.WithResponseFormat(&types.ResponseFormat{Type: "json_object"}),
						)
						if cErr != nil {
							candidateCh <- candidateResult{}
							return
						}
						// GR-4-005 复核修复：PRM 候选路径此前只传 json_object 格式提示，
						// 无 Schema 约束，字段类型错误的候选会被 json.Unmarshal 静默解析成
						// 带零值字段的“看似合法”的 DAGModel。加一层结构校验，未通过的候选
						// 按原有语义直接作废（不占用 candidates 切片，不影响 PRM 选优）。
						if schemaErr := schemavalidate.Validate(llmEff.SchemaRef, []byte(cResp.Content)); schemaErr != nil {
							metrics.GlobalSchemaValidationFailureTotal.Add(1)
							slog.Warn("agent: PRM candidate failed schema validation, discarding",
								"schema_ref", llmEff.SchemaRef, "err", schemaErr)
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
					})
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
			reqMsgs, err = a.tokenizeMessagesForLLM(ctx, reqMsgs)
			if err != nil {
				return apperr.Wrap(apperr.CodeInternal, "agent: failed to tokenize messages, fail-closed", err)
			}
			ch, streamErr := safecall.StreamInfer(ctx, a.provider, reqMsgs, types.WithModel(llmEff.ModelPool), types.WithThinkingMode(llmEff.ThinkingMode))
			if streamErr != nil {
				inferErr = streamErr
			} else {
				resp, inferErr = a.doStreamInfer(ctx, ch)
			}

			if inferErr != nil {
				if errors.Is(inferErr, protocol.ErrAllProvidersFailed) {
					a.sCtx.ProviderSuspendCount++
					if a.sCtx.ProviderSuspendCount >= 5 && a.hitl != nil {
						hitlResp, hitlErr := a.hitl.Prompt(ctx, types.HITLPrompt{
							ID:             fmt.Sprintf("hitl_%d", time.Now().UnixNano()),
							AgentID:        a.sCtx.AgentID,
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
				actualTokens := resp.Usage.InputTokens + resp.Usage.OutputTokens
				a.sCtx.TokensInput += resp.Usage.InputTokens
				a.sCtx.TokensOutput += resp.Usage.OutputTokens
				a.sCtx.TokensCacheRead += resp.Usage.CacheHitTokens
				a.sCtx.TokensUsed += actualTokens
				// [Task 11] BudgetManager 会话级记账：使用实际 token 数进行全局计账。
				if a.sCtx.Budget != nil {
					if budgetErr := a.sCtx.Budget.ConsumeTokens(actualTokens); budgetErr != nil {
						slog.Warn("kernel: session budget exceeded via BudgetManager",
							"agent_id", a.ID, "tokens", actualTokens, "err", budgetErr)
						a.sm.ForceState(types.AgentStateFailed)
						return apperr.Wrap(apperr.CodeInternal, "BudgetManager.ConsumeTokens", budgetErr)
					}
				}
				// 保存 reasoning_content 供下轮消息历史回传（BUG-04 fix）
				if resp.ReasoningContent != "" {
					a.sCtx.LastReasoningContent = resp.ReasoningContent
				}

				// GR-4-005 复核修复：仅做可观测性埋点，不在此处改变控制流。
				// 原本考虑过在这里校验失败时直接短路到 OnFailure，但 OnSuccess 的具体实现
				// （fsm.parsePlanOnSuccess / onReflectSuccess）本身已经内建了"unmarshal 失败
				// 时复用上一轮缓存 DAGModel，缓存也没有才判定为硬失败"的降级语义（S_PLAN /
				// S_REFLECT 的既有测试直接依赖这个行为）。在这里短路会绕开那套已经调好的降级
				// 逻辑，属于用一个新问题替换旧问题。真正的校验强制在各 OnSuccess 内部就近实现
				// （见 fsm.parsePlanOnSuccess / onReflectSuccess），与既有的 unmarshal 失败分支
				// 合并处理，保证只有一套"内容不可用"的判定与降级路径。这里只负责让运维能看到
				// 校验失败发生过，不代表内容一定被拒绝。
				if schemaErr := schemavalidate.Validate(llmEff.SchemaRef, []byte(resp.Content)); schemaErr != nil {
					metrics.GlobalSchemaValidationFailureTotal.Add(1)
					slog.Warn("agent: LLMFillEffect response failed schema validation (see OnSuccess for actual degradation handling)",
						"schema_ref", llmEff.SchemaRef, "state", a.sm.Current(), "err", schemaErr)
				}
				nextState, err = llmEff.OnSuccess(a.toProtocolCtx(), []byte(resp.Content))
			}
		}

	HANDLE_MEM:
		// 记忆落盘逻辑拆分至 recordLLMFillEffectMemory（R7 文件行数治理，2026-07-07），
		// goto 落点保持不变，仅将 label 内的语句体移出为独立方法，行为不变。
		a.recordLLMFillEffectMemory(ctx, nextState, resp)
	} else {
		var handled bool
		nextState, err, handled = a.executeDeterministicEffect(ctx, effect)
		if handled {
			return err
		}
	}

	// 优先判断是否有逻辑状态推进。如果有，说明 FSM 已经接管了这个业务错误，我们不抛出致命异常
	if nextState != "" {
		if trigger, ok := stateToTriggerMap()[nextState]; ok {
			a.asyncIntent(trigger)
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

const (
	budgetWarnPct = 50
)
