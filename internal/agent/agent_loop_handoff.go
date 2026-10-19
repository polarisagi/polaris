package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/types"
)

type ctxInBatchDelegationKey struct{}
type ctxToolCallIDKey struct{}

func withToolCallID(ctx context.Context, callID string) context.Context {
	return context.WithValue(ctx, ctxToolCallIDKey{}, callID)
}

func getToolCallID(ctx context.Context) string {
	if val := ctx.Value(ctxToolCallIDKey{}); val != nil {
		if id, ok := val.(string); ok {
			return id
		}
	}
	return ""
}

// isDelegatingWaitAll 判定单条工具调用是否为需挂起等待的委派调用。
func isDelegatingWaitAll(call types.InferToolCall) bool {
	if call.Name == "transfer_to_agent" {
		return true
	}
	if call.Name == "spawn_agents" {
		var args types.SpawnAgentsArgs
		if err := json.Unmarshal(call.Input, &args); err == nil {
			return strings.ToLower(strings.TrimSpace(args.Wait)) != "none"
		}
		return true
	}
	return false
}

// executeBatchWithDelegation 校验并执行当前批次工具调用，处理委派挂起（ADR-0115 WP4 / R2-03 / R3-05）。
func (a *Agent) executeBatchWithDelegation(ctx context.Context, calls []types.InferToolCall) (bool, error) {
	plan := toolCallsToDAGPlan(calls, a.sCtx.GlobalTaintLevel)
	if err := a.checkPlanAgainstProfile(plan); err != nil {
		slog.WarnContext(ctx, "agentic_loop: plan rejected by profile", "err", err)
		a.appendToolErrorMessages(calls, "Forbidden: tool violates agent profile policy: "+err.Error())
		a.asyncIntent(types.TriggerLoopExecDone)
		return false, nil
	}

	vCtx := a.buildValidationContext(plan)
	if a.dagValidator == nil {
		slog.ErrorContext(ctx, "agentic_loop: security validator is missing (fail-closed)")
		a.appendToolErrorMessages(calls, "Internal: security validator is missing (fail-closed)")
		a.asyncIntent(types.TriggerLoopExecDone)
		return false, nil
	}
	if err := a.validateWithTaintReview(ctx, vCtx); err != nil {
		slog.WarnContext(ctx, "agentic_loop: plan rejected by taint review", "err", err)
		a.appendToolErrorMessages(calls, "Forbidden: action rejected by security policy: "+err.Error())
		a.asyncIntent(types.TriggerLoopExecDone)
		return false, nil
	}
	a.recordValidatedPlan(plan)

	var nonDelegatingCalls []types.InferToolCall
	var delegatingCalls []types.InferToolCall
	for i := range calls {
		if isDelegatingWaitAll(calls[i]) {
			delegatingCalls = append(delegatingCalls, calls[i])
		} else {
			nonDelegatingCalls = append(nonDelegatingCalls, calls[i])
		}
	}

	if len(delegatingCalls) == 0 {
		toolResults := a.executeToolCallsConcurrently(ctx, calls)
		a.sCtx.Mu.Lock()
		a.sCtx.LoopMessages = append(a.sCtx.LoopMessages, toolResults...)
		a.sCtx.Mu.Unlock()
		return false, nil
	}

	var nonDelegatingResults []types.Message
	if len(nonDelegatingCalls) > 0 {
		nonDelegatingResults = a.executeToolCallsConcurrently(ctx, nonDelegatingCalls)
	}

	a.sCtx.Mu.Lock()
	a.sCtx.LoopHandoffPendingResults = nonDelegatingResults
	a.sCtx.LoopHandoffToolCalls = calls
	if a.sCtx.LoopHandoffSpawnCallID == nil {
		a.sCtx.LoopHandoffSpawnCallID = make(map[string][]string)
	}
	a.sCtx.Mu.Unlock()

	delMsgs := make(map[string]types.Message)
	for _, delCall := range delegatingCalls {
		delCtx := context.WithValue(ctx, ctxInBatchDelegationKey{}, true)
		delCtx = withToolCallID(delCtx, delCall.ID)
		msg := a.executeLoopToolCall(delCtx, delCall)
		delMsgs[delCall.ID] = msg
	}

	a.sCtx.Mu.Lock()
	allChildIDs := append([]string(nil), a.sCtx.HandoffTaskIDs...)
	isSuspended := len(allChildIDs) > 0
	a.sCtx.Mu.Unlock()

	if isSuspended {
		a.persistLoopRoundDelta(ctx)
		a.watchSubagentsCompletion(allChildIDs)
		a.asyncIntent(types.TriggerAwaitAgent)
		return true, nil
	}

	a.sCtx.Mu.Lock()
	a.sCtx.LoopHandoffPendingResults = nil
	a.sCtx.LoopHandoffToolCalls = nil
	a.sCtx.LoopHandoffSpawnCallID = nil
	a.sCtx.Mu.Unlock()

	batchMessages := assembleResumeBatchMessages(calls, delMsgs, nonDelegatingResults)
	a.sCtx.Mu.Lock()
	a.sCtx.LoopMessages = append(a.sCtx.LoopMessages, batchMessages...)
	a.sCtx.Mu.Unlock()
	return false, nil
}

// resumeHandoffInLoop 在 S_LOOP_AWAIT 唤醒回到 S_LOOP_EXEC 时回填子智能体结果并进入下一轮循环（R2-03 / R3-05）。
func (a *Agent) resumeHandoffInLoop(ctx context.Context) error {
	a.sCtx.Mu.Lock()
	spawnCallMap := a.sCtx.LoopHandoffSpawnCallID
	origCalls := a.sCtx.LoopHandoffToolCalls
	pendingResults := a.sCtx.LoopHandoffPendingResults
	currentRound := a.sCtx.LoopRound
	a.sCtx.Mu.Unlock()

	spawnMsgs := make(map[string]types.Message)
	for spawnCallID, childIDs := range spawnCallMap {
		childResults, highestTaint := a.fetchSubagentsResults(ctx, childIDs)

		var spawnResultText string
		if len(childResults) == 1 {
			spawnResultText = childResults[0]
		} else if len(childResults) > 1 {
			var b strings.Builder
			for i, r := range childResults {
				if i > 0 {
					b.WriteString("\n\n")
				}
				fmt.Fprintf(&b, "[%s]: %s", childIDs[i], r)
			}
			spawnResultText = b.String()
		} else {
			spawnResultText = "[subagent completed with empty output]"
		}

		spawnMsg := makeToolResultMessage(spawnCallID, spawnResultText)
		spawnMsg.Round = currentRound
		spawnMsg.TaintLevel = highestTaint
		if highestTaint > types.TaintNone {
			a.elevateGlobalTaintLevel(highestTaint)
		}
		spawnMsgs[spawnCallID] = spawnMsg
	}

	finalBatchMessages := assembleResumeBatchMessages(origCalls, spawnMsgs, pendingResults)

	a.cancelHandoffWatcher()
	a.sCtx.Mu.Lock()
	a.sCtx.LoopMessages = append(a.sCtx.LoopMessages, finalBatchMessages...)
	a.sCtx.HandoffTaskIDs = nil
	a.sCtx.HandoffTaskID = ""
	a.sCtx.LoopHandoffPendingResults = nil
	a.sCtx.LoopHandoffToolCalls = nil
	a.sCtx.LoopHandoffSpawnCallID = nil
	a.sCtx.Mu.Unlock()

	a.applyLoopObservationMasking(ctx)
	steers := a.drainSteerQueue()
	if len(steers) > 0 {
		a.sCtx.Mu.Lock()
		for _, st := range steers {
			a.sCtx.LoopMessages = append(a.sCtx.LoopMessages, types.Message{
				Role:    "user",
				Content: st,
			})
		}
		a.sCtx.Mu.Unlock()
	}
	metrics.RecordLoopRound(ctx)
	a.persistLoopRoundDelta(ctx)

	a.asyncIntent(types.TriggerLoopExecDone)
	return nil
}

// fetchSubagentsResults 读取各子智能体执行产物并做截断与卸载（T5）。
func (a *Agent) fetchSubagentsResults(ctx context.Context, handoffTaskIDs []string) ([]string, types.TaintLevel) {
	childResults := make([]string, 0, len(handoffTaskIDs))
	highestTaint := types.TaintNone
	for _, childID := range handoffTaskIDs {
		if a.handoffPoster == nil {
			childResults = append(childResults, fmt.Sprintf("subagent %s: handoff poster missing", childID))
			continue
		}
		snap, err := a.handoffPoster.PeekTask(ctx, childID)
		if err != nil || snap == nil {
			childResults = append(childResults, fmt.Sprintf("subagent %s: result not found", childID))
			continue
		}
		role := strings.TrimPrefix(snap.Type, "agent_handoff:")
		roleTaint := resolveHandoffResultTaint(role, a.sCtx.GlobalTaintLevel)
		if roleTaint > highestTaint {
			highestTaint = roleTaint
		}
		if snap.Status == types.TaskFailed {
			childResults = append(childResults, fmt.Sprintf("subagent %s failed: %s", childID, string(snap.Result)))
		} else {
			resStr := string(snap.Result)
			truncated := a.truncateAndOffloadResult(ctx, childID, resStr, 4000)
			childResults = append(childResults, truncated)
		}
	}
	return childResults, highestTaint
}

// assembleResumeBatchMessages 按照原始调用序列拼装工具调用结果（保持 tool_use_id 对应）。
func assembleResumeBatchMessages(origCalls []types.InferToolCall, spawnMsgs map[string]types.Message, pendingResults []types.Message) []types.Message {
	pendingMap := make(map[string]types.Message)
	for _, msg := range pendingResults {
		for _, part := range msg.Parts {
			if pMap, ok := part.(map[string]any); ok && pMap["type"] == "tool_result" {
				if id, ok := pMap["tool_use_id"].(string); ok {
					pendingMap[id] = msg
				}
			}
		}
	}

	finalBatchMessages := make([]types.Message, 0, len(origCalls))
	for _, c := range origCalls {
		if msg, ok := spawnMsgs[c.ID]; ok {
			finalBatchMessages = append(finalBatchMessages, msg)
		} else if m, ok := pendingMap[c.ID]; ok {
			finalBatchMessages = append(finalBatchMessages, m)
		} else {
			finalBatchMessages = append(finalBatchMessages, makeToolResultMessage(c.ID, "[completed]"))
		}
	}
	if len(finalBatchMessages) == 0 {
		for _, msg := range spawnMsgs {
			finalBatchMessages = append(finalBatchMessages, msg)
		}
		finalBatchMessages = append(finalBatchMessages, pendingResults...)
	}
	return finalBatchMessages
}
