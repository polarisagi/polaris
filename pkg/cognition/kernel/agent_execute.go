package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

// budgetWarnPct / budgetCriticalPct：Token 预算分级阈值（百分比，整数）。
// 50% → 告警日志；75% → BudgetPressure 标记（S_PLAN 收紧 DAG）；100% → INFERENCE_OOM 硬失败。
const (
	budgetWarnPct     = 50
	budgetCriticalPct = 75
)

func (a *Agent) executeEffect(ctx context.Context, effect protocol.Effect) error { //nolint:gocyclo
	var nextState protocol.State
	var err error

	if effect.IsLLMFill() { //nolint:nestif
		llmEff, ok := effect.(protocol.LLMFillEffect)
		if !ok {
			return perrors.New(perrors.CodeInternal, "invalid LLMFillEffect type")
		}

		// 1. Budget Control: 分级预算检查
		if a.sCtx.TokenBudget > 0 {
			used := a.sCtx.TokensUsed
			budget := a.sCtx.TokenBudget

			switch {
			case used > budget:
				// 硬断路：INFERENCE_OOM，任务失败。
				// M3 在下一轮 TokenBurnRate 检测时自驱触发 KillSwitch CheckAndAct。
				a.sm.history = append(a.sm.history, a.sm.current)
				a.sm.current = protocol.AgentStateFailed
				return perrors.New(perrors.CodeInternal,
					fmt.Sprintf("INFERENCE_OOM: token budget exceeded (%d > %d)", used, budget))

			case used*100/budget >= budgetCriticalPct:
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
			return perrors.New(perrors.CodeInternal, "agent missing provider for LLMFillEffect")
		}

		var resp *protocol.ProviderResponse
		var inferErr error

		// 2. System 1/2 Routing & World Model Inference Skip
		// 如果在 S_PERCEIVE 阶段，且 SurpriseIndex 很低 (<0.3)，走 FastPath 跳过 LLM
		// System I 快思考路径：SurpriseIndex < 0.3 时直接旁路 LLM，对应三轨推理引擎的"降维"轨道。
		if a.sm.Current() == protocol.AgentStatePerceive {
			// FastPath (M09 Logic Collapse): 跳过 S_PERCEIVE LLM 推理，产生合成感知结果。
			// 不操作 DAGModel/ExecuteResult——外部注入的 DAGModel 保持不变。
			// 具体能力识别将在后续通过 intent 向量检索实现（ADR-M09）。
			// 注意: SurpriseIndex == 0 表示"未计算"（默认值），不触发 FastPath。
			if a.sCtx.SurpriseIndex > 0 && a.sCtx.SurpriseIndex < 0.3 {

				fastResult := `{"Goal":` + mustMarshalString(a.sCtx.RawIntentTS.Content()) + `,"Complexity":0.1}`
				nextState, err = llmEff.OnSuccess(protocol.StateContext{}, []byte(fastResult))
				goto HANDLE_MEM
			}
		}

		// S_PLAN 阶段
		if a.sm.Current() == protocol.AgentStatePlan {
			if a.sCtx.SurpriseIndex > 0 && a.sCtx.SurpriseIndex < 0.3 {
				// FastPath 路径：S_PERCEIVE 已坍缩，直接旁路 LLM 规划。
				// DAGModel 为 nil 时 runExecuteDAG 直接推进 ExecuteDone，
				// 为非 nil 时执行已生成的 DAG（高置信路径）。
				// SurpriseIndex == 0 表示"未计算"，不触发。
				nextState = "S_PLAN_DONE"
				err = nil
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

				type candidateResult struct {
					plan   *protocol.DAGModel
					tokens int
				}
				candidateCh := make(chan candidateResult, n)

				for range n {
					go func() {
						cResp, cErr := a.provider.Infer(ctx, baseMessages, protocol.WithModel(llmEff.ModelPool), protocol.WithThinkingMode(llmEff.ThinkingMode))
						if cErr != nil {
							candidateCh <- candidateResult{}
							return
						}
						var plan protocol.DAGModel
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

				var candidates []*protocol.DAGModel
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
					resp = &protocol.ProviderResponse{Content: string(bestJSON)}
					nextState, err = llmEff.OnSuccess(a.toProtocolCtx(), bestJSON)
					goto HANDLE_MEM
				}
				// 所有候选均失败时降级到单次 Infer
			}
		}

		{
			reqMsgs := llmEff.PromptFn(a.toProtocolCtx())
			resp, inferErr = a.provider.Infer(ctx, reqMsgs, protocol.WithModel(llmEff.ModelPool), protocol.WithThinkingMode(llmEff.ThinkingMode))
			if inferErr != nil {
				nextState, err = llmEff.OnFailure(a.toProtocolCtx(), inferErr)
			} else {
				// 累计分项 Token（Gap-A：分开记录供 Worker 写回 Blackboard）
				a.sCtx.TokensInput += resp.Usage.InputTokens
				a.sCtx.TokensOutput += resp.Usage.OutputTokens
				a.sCtx.TokensCacheRead += resp.Usage.CacheHitTokens
				a.sCtx.TokensUsed += resp.Usage.InputTokens + resp.Usage.OutputTokens
				nextState, err = llmEff.OnSuccess(a.toProtocolCtx(), []byte(resp.Content))
			}
		}

	HANDLE_MEM:
		// 成功完成感知，将用户意图作为事件写入记忆（由于当前缺失 TaskIntent，仅做预留演示）
		if nextState == "S_PERCEIVE_DONE" && a.memory != nil && (resp != nil || a.sCtx.SurpriseIndex < 0.3) {
			var content string
			if resp != nil {
				content = resp.Content
			} else {
				content = `{"Goal": "` + a.sCtx.RawIntentTS.Content() + `", "Complexity": 0.1}`
			}
			_ = a.memory.Episodic().Append(ctx, protocol.Event{
				ID:        a.sm.nextEventID(a.sCtx.SessionID, "perceive"),
				Type:      "task_perceived",
				Payload:   []byte(content),
				CreatedAt: time.Now(),
			})
		}

		// 成功完成计划，写入计划记忆
		if nextState == "S_PLAN_DONE" && a.memory != nil && (resp != nil || a.sCtx.SurpriseIndex < 0.3) {
			var content string
			if resp != nil {
				content = resp.Content
			} else if a.sCtx.DAGModel != nil {
				planBytes, _ := json.Marshal(a.sCtx.DAGModel)
				content = string(planBytes)
			}
			_ = a.memory.Episodic().Append(ctx, protocol.Event{
				ID:        a.sm.nextEventID(a.sCtx.SessionID, "plan"),
				Type:      "plan_generated",
				Payload:   []byte(content),
				CreatedAt: time.Now(),
			})
		}

		// 成功完成反思，写入反思记忆，并保存 ReasoningState
		if nextState == "S_REFLECT_DONE" && a.memory != nil && (resp != nil || a.sCtx.SurpriseIndex < 0.3) {
			var content string
			if resp != nil {
				content = resp.Content
			}
			// 保存至内存上下文以供跨轮次携带
			a.sCtx.ReasoningState = []byte(content)

			_ = a.memory.Episodic().Append(ctx, protocol.Event{
				ID:        a.sm.nextEventID(a.sCtx.SessionID, "reflect"),
				Type:      "reflection_completed",
				Payload:   []byte(content),
				CreatedAt: time.Now(),
			})
		}
	} else {
		detEff, ok := effect.(protocol.DeterministicEffect)
		if !ok {
			return perrors.New(perrors.CodeInternal, "invalid DeterministicEffect type")
		}

		// S_VALIDATE 阶段拦截：调用 Agent 层四层校验（可访问 policyGate 与完整 sCtx）。
		// 此分支由 runValidateDAG 自行通过 SendIntent 推进 FSM（ValidateOk / ValidateFail），
		// 因此直接返回，不走 stateToTriggerMap 路径，避免双重推进。
		if a.sm.Current() == protocol.AgentStateValidate {
			// FastPath 空执行路径（SurpriseIndex 触发但无 LLM 生成 DAG）：nil DAGModel 直接放行。
			// runValidateDAG 对 nil plan 会触发 L0 拦截，因此在进入前短路。
			if a.sCtx.DAGModel == nil {
				go func() { _ = a.SendIntent(protocol.TriggerValidateOk) }()
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
		if a.sm.Current() == protocol.AgentStateExecute {
			// runExecuteDAG 内负责在完成后将结果写入 a.sCtx.ExecuteResult
			err := a.runExecuteDAG(ctx)
			if err == nil && a.memory != nil && len(a.sCtx.ExecuteResult) > 0 {
				_ = a.memory.Episodic().Append(ctx, protocol.Event{
					ID:        a.sm.nextEventID(a.sCtx.SessionID, "exec"),
					Type:      "execution_completed",
					Payload:   a.sCtx.ExecuteResult,
					CreatedAt: time.Now(),
				})
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
		if trigger, ok := stateToTriggerMap[nextState]; ok {
			go func() { _ = a.SendIntent(trigger) }()
			return nil
		}
		return perrors.New(perrors.CodeInternal, fmt.Sprintf("unknown next state: %s (err: %v)", nextState, err))
	}

	// 只有当没有状态流转时，才把底层技术错误抛出导致 Agent 终止
	if err != nil {
		return err
	}

	return nil
}

// runValidateDAG 是 Agent 层面的四层校验入口。
// 与 StateMachine.validateDAG 的区别在于：
//   - 能够访问 a.sCtx.DAGModel（LLM 产出的 DAG）
//   - 能够访问 a.policyGate（Cedar 引擎）
//   - 返回结构化的 State 令牌以推进 FSM
func (a *Agent) runValidateDAG(ctx context.Context) error {
	var plan *DAGPlan
	if a.sCtx.DAGModel != nil {
		plan = &DAGPlan{
			Nodes: a.sCtx.DAGModel.Nodes,
			Edges: a.sCtx.DAGModel.Edges,
		}
	}

	vCtx := &DAGValidationContext{
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

	if err := ValidateDAG(ctx, vCtx); err != nil {
		// 校验失败→ 异步推送 TriggerValidateFail 以面向 FSM 的 S_REPLAN
		go func() { _ = a.SendIntent(protocol.TriggerValidateFail) }()
		// 返回非致命 error 提示调用方失败原因，但不能让 Run 循环崩溃
		return perrors.Wrap(perrors.CodeInternal, "s_validate failed", err)
	}

	// L3: LLM 看门狗校验 (上提为标准 FSM Effect)
	// 仅对 Tier 1+ 生效
	if vCtx.SystemTier >= 1 && a.provider != nil && vCtx.Plan != nil {
		var dangerous []string
		for _, node := range vCtx.Plan.Nodes {
			if !isReadOnlyTool(node.ToolName, a.toolRegistry) {
				dangerous = append(dangerous, fmt.Sprintf("Tool: %s, Args: %s", node.ToolName, string(node.Args)))
			}
		}

		if len(dangerous) > 0 {
			prompt := "Review the following tool executions for security risks:\n" + strings.Join(dangerous, "\n") + "\n\nIf it's dangerous, reply with 'DENY: <reason>'. Otherwise reply 'ALLOW'."

			llmEff := protocol.LLMFillEffect{
				SchemaRef: "l3_watchdog",
				PromptFn: func(pCtx protocol.StateContext) []protocol.Message {
					return []protocol.Message{
						{Role: "system", Content: "You are a strict security watchdog."},
						{Role: "user", Content: prompt},
					}
				},
				OnSuccess: func(pCtx protocol.StateContext, content []byte) (protocol.State, error) {
					if strings.HasPrefix(strings.ToUpper(string(content)), "DENY") {
						go func() { _ = a.SendIntent(protocol.TriggerValidateFail) }()
						return "S_VALIDATE_FAIL", perrors.New(perrors.CodeForbidden, "LLM Watchdog denied: "+string(content))
					}
					go func() { _ = a.SendIntent(protocol.TriggerValidateOk) }()
					return "S_VALIDATE_OK", nil
				},
				OnFailure: func(pCtx protocol.StateContext, err error) (protocol.State, error) {
					// L3 失败时 fail-open——架构设计，非疏漏。
					// 依据: M04 §L3 LLM 看门狗: "LLM 不可用时 fail-open 推进 S_VALIDATE_OK"。
					// L3 是补充信号层：L0/L1/L2 未放行的动作不可因 L3 通过而放行；
					// L3 DENY 推进 ValidateFail，L3 LLM 不可用时不应因此阻断正常业务流。
					// 禁止改为 fail-closed：L3 LLM 故障会导致所有非只读 DAG 永久卡住。
					go func() { _ = a.SendIntent(protocol.TriggerValidateOk) }()
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
	go func() { _ = a.SendIntent(protocol.TriggerValidateOk) }()
	return nil
}

// runExecuteDAG 是 Agent 层面的 DAG 执行入口。
// 从 a.sCtx.DAGModel 构建 DAGPlan，通过 DAGExecutor 按拓扑序并发执行工具，
// 结果写入 a.sCtx.ExecuteResult。
// 任意节点失败 → 推送 TriggerExecuteFail（触发 S_ROLLBACK 和 Saga 补偿）。
func (a *Agent) runExecuteDAG(ctx context.Context) error { //nolint:gocyclo
	if a.sCtx.DAGModel == nil {
		// DAGModel 为空时跳过执行（等价于空 DAG），直接推进 ExecuteDone
		go func() { _ = a.SendIntent(protocol.TriggerExecuteDone) }()
		return nil
	}

	if a.toolRegistry == nil {
		// fail-closed: 无工具注册表时拒绝执行
		go func() { _ = a.SendIntent(protocol.TriggerExecuteFail) }()
		return perrors.New(perrors.CodeInternal, "runExecuteDAG: toolRegistry is nil (fail-closed)")
	}

	plan := &DAGPlan{
		Nodes: a.sCtx.DAGModel.Nodes,
		Edges: a.sCtx.DAGModel.Edges,
	}

	// 将 ToolRegistry.ExecuteTool 绑定为 DAGExecutor 的工具执行函数
	toolExecFn := func(ctx context.Context, toolName string, args []byte, taintLevel protocol.TaintLevel) (*protocol.ToolResult, error) {
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
			go func() { _ = a.SendIntent(protocol.TriggerInterruptReceived) }()

			return &protocol.ToolResult{
				Success: true,
				Output:  []byte("Planner pool spawned, agent suspended waiting for whisper."),
			}, nil
		}

		tool, err := a.toolRegistry.Lookup(toolName)
		isIdempotent := true
		if err == nil {
			for _, se := range tool.SideEffects {
				if se != protocol.SideNone {
					isIdempotent = false
					break
				}
			}
		}

		var pendingEventID string
		// [2PC Phase 1] 检查是否曾意外崩溃，并预写日志
		if !isIdempotent { //nolint:nestif
			query := protocol.EpisodicQuery{
				SessionID: a.sCtx.SessionID,
			}
			events, err := a.memory.Episodic().Query(ctx, query)
			if err == nil {
				hasPending := false
				hasDone := false
				signature := fmt.Sprintf(`"tool":"%s"`, toolName)
				for _, e := range events {
					if strings.Contains(string(e.Event.Payload), signature) {
						if e.Event.Type == protocol.EventActionPending {
							hasPending = true
						} else if e.Event.Type == protocol.EventActionDone {
							hasDone = true
						}
					}
				}
				if hasPending && !hasDone {
					// Crashed during execution. 阻断以防止外部副作用重复发生
					return nil, perrors.New(perrors.CodeInternal, fmt.Sprintf("double execution prevented: non-idempotent tool %s was interrupted previously", toolName))
				}
				if hasDone {
					// 已成功执行但后续环节崩溃导致重跑
					return &protocol.ToolResult{
						Success: true,
						Output:  []byte(fmt.Sprintf("tool %s was already executed successfully", toolName)),
					}, nil
				}
			}

			// 写预写日志 Action_Pending
			pendingEventID = uuid.New().String()
			_ = a.memory.Episodic().Append(ctx, protocol.Event{
				ID:        pendingEventID,
				Type:      protocol.EventActionPending,
				Status:    protocol.StatusExecuting,
				TaskID:    a.sCtx.SessionID,
				AgentID:   a.sCtx.AgentID,
				Payload:   []byte(fmt.Sprintf(`{"tool":"%s","args":%s}`, toolName, string(args))),
				CreatedAt: time.Now(),
			})
		}

		// HITL 拦截逻辑 (Computer Use Confirmations Policy)
		if errHITL := a.interceptComputerUse(ctx, toolName, args); errHITL != nil {
			//nolint:nilerr // ToolExecutor expects error to be reported in ToolResult for LLM to see
			return &protocol.ToolResult{
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
			status := protocol.StatusDone
			if err != nil || (res != nil && !res.Success) {
				status = protocol.StatusFailed
			}
			_ = a.memory.Episodic().Append(ctx, protocol.Event{
				ID:        uuid.New().String(),
				Type:      protocol.EventActionDone,
				Status:    status,
				TaskID:    a.sCtx.SessionID,
				AgentID:   a.sCtx.AgentID,
				Payload:   []byte(fmt.Sprintf(`{"tool":"%s","status":"%s"}`, toolName, status)),
				CreatedAt: time.Now(),
			})
		}

		return res, err
	}

	executor := NewDAGExecutor(toolExecFn, nil) // leaseRenew 由 M8 注入，MVP 传 nil
	results, err := executor.Execute(ctx, plan, a.sCtx.SessionID, a.sCtx.AgentID)

	if err != nil {
		if strings.Contains(err.Error(), "tool not found") {
			a.sCtx.SuspendReason = "capability_gap"

			// 通过 outbox 异步投递 m9_capability_gap 事件，触发 GapFillWorker 进行能力补全
			if a.db != nil {
				payloadBytes, _ := json.Marshal(map[string]string{"error": err.Error()})
				_, _ = a.db.ExecContext(ctx, `
					INSERT INTO outbox (created_at, target_engine, operation, scope, payload, idempotency_key, status)
					VALUES (?, ?, ?, ?, ?, ?, ?)
				`, time.Now().UnixMilli(), "m9_capability_gap", "upsert", "capability_gap", payloadBytes, uuid.New().String(), "pending")
			}

			go func() { _ = a.SendIntent(protocol.TriggerInterruptReceived) }()
			return nil
		}

		// 执行失败 → 触发 S_ROLLBACK
		go func() { _ = a.SendIntent(protocol.TriggerExecuteFail) }()
		return perrors.Wrap(perrors.CodeInternal, "runExecuteDAG: DAG execution failed", err)
	}

	// 聚合所有节点输出为 JSON 数组，反思阶段可获取完整 DAG 执行结果。
	// 单节点时保持向后兼容（直接取 output 字节）；多节点时序列化为 {"results":[...]} 结构。
	a.sCtx.ExecuteResult = aggregateDAGResults(results)
	go func() { _ = a.SendIntent(protocol.TriggerExecuteDone) }()
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
		prompt := protocol.HITLPrompt{
			ID:             uuid.New().String(),
			CheckpointType: "security_review",
			PromptText:     fmt.Sprintf("Agent requests to execute %s with args: %s\nMode: %s", toolName, string(args), mode),
			Options: []protocol.HITLOption{
				{Key: "approve", Label: "Approve"},
				{Key: "deny", Label: "Deny"},
			},
			DeadlineNs: time.Now().Add(5 * time.Minute).UnixNano(),
		}
		respHITL, hitlErr := a.hitl.Prompt(ctx, prompt)
		if hitlErr != nil || respHITL == nil || respHITL.OptionKey != "approve" {
			if hitlErr != nil {
				return perrors.Wrap(perrors.CodeForbidden, "HITL gateway denied computer use action", hitlErr)
			}
			return perrors.New(perrors.CodeForbidden, "HITL gateway denied computer use action")
		}
	}
	return nil
}

// aggregateDAGResults 将多节点执行结果聚合为统一 JSON 格式。
// 单节点直接返回 output；多节点序列化为 {"results":[{id,output},...]}.
func aggregateDAGResults(results []NodeResult) []byte {
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

// mustMarshalString 将字符串 JSON 安全序列化为带引号的 JSON 字符串字面量。
// 用于拼接 FastPath JSON payload，避免直接字符串拼接导致的 JSON 注入风险。
func mustMarshalString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// maxNodeTaintLevel 计算 DAGPlan 中所有节点的最高污点等级。
// 实现 ADR-0007 PropagateTaint 语义：output = max(inputs)，只升不降。
// plan 为 nil 或无节点时返回 TaintNone（validateTaintGate 自动跳过）。
func maxNodeTaintLevel(plan *DAGPlan) protocol.TaintLevel {
	if plan == nil {
		return protocol.TaintNone
	}
	var max protocol.TaintLevel
	for _, node := range plan.Nodes {
		if node.TaintLevel > max {
			max = node.TaintLevel
		}
	}
	return max
}
