package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// persistLoopRoundDelta 落盘 agentic 循环当前轮次的新增增量消息与元数据（R2-05 / ADR-0104）。
// 每轮 LLM 响应生成后、以及工具执行结果追加后调用。
func (a *Agent) persistLoopRoundDelta(ctx context.Context) {
	if a == nil || a.extQuerier == nil || a.sCtx == nil {
		return
	}
	a.sCtx.Mu.RLock()
	round := a.sCtx.LoopRound
	sessionID := a.sCtx.SessionID
	turnID := a.sCtx.TurnID
	globalTaint := a.sCtx.GlobalTaintLevel
	allMessages := a.sCtx.LoopMessages
	a.sCtx.Mu.RUnlock()

	if round <= 0 || sessionID == "" || turnID == "" {
		return
	}

	// 提取属于本轮的增量消息（第 1 轮包含初始 user 意图与任何前置消息）
	var delta []types.Message
	for _, m := range allMessages {
		if m.Round == round || (round == 1 && m.Round <= 1) {
			delta = append(delta, m)
		}
	}
	if len(delta) == 0 {
		return
	}

	// 数据卫生：对 secret_get 明文结果做持久化脱敏（R2-05）
	redactedDelta := redactSecretGetForPersistence(delta)
	deltaBytes, err := json.Marshal(redactedDelta)
	if err != nil {
		slog.WarnContext(ctx, "agentic_loop: marshal round delta failed",
			"session_id", sessionID, "turn_id", turnID, "round", round, "err", err)
		return
	}

	nowMs := time.Now().UnixMilli()
	query := `
		INSERT INTO agent_loop_rounds (session_id, turn_id, round, status, delta_json, taint_level, created_at)
		VALUES (?, ?, ?, 'running', ?, ?, ?)
		ON CONFLICT(session_id, turn_id, round) DO UPDATE SET
			delta_json = excluded.delta_json,
			taint_level = excluded.taint_level,
			status = 'running'`
	if _, execErr := a.extQuerier.ExecContext(ctx, query, sessionID, turnID, round, string(deltaBytes), int(globalTaint), nowMs); execErr != nil {
		slog.WarnContext(ctx, "agentic_loop: persist agent_loop_rounds failed",
			"session_id", sessionID, "turn_id", turnID, "round", round, "err", execErr)
	}
}

// markLoopTurnStatus 将指定 turn 的所有轮次状态更新为 done 或 aborted（R2-05）。
func (a *Agent) markLoopTurnStatus(ctx context.Context, status string) {
	if a == nil || a.extQuerier == nil || a.sCtx == nil {
		return
	}
	a.sCtx.Mu.RLock()
	sessionID := a.sCtx.SessionID
	turnID := a.sCtx.TurnID
	a.sCtx.Mu.RUnlock()

	if sessionID == "" || turnID == "" {
		return
	}

	query := `UPDATE agent_loop_rounds SET status = ? WHERE session_id = ? AND turn_id = ?`
	if _, err := a.extQuerier.ExecContext(ctx, query, status, sessionID, turnID); err != nil {
		slog.WarnContext(ctx, "agentic_loop: update turn status failed",
			"session_id", sessionID, "turn_id", turnID, "status", status, "err", err)
	}
}

// redactSecretGetForPersistence 将 secret_get 工具调用的明文结果替换为 [secret:<name> redacted]（R2-05）。
func redactSecretGetForPersistence(messages []types.Message) []types.Message {
	if len(messages) == 0 {
		return nil
	}
	secretCalls := extractSecretCallMap(messages)
	if len(secretCalls) == 0 {
		return messages
	}

	redacted := make([]types.Message, len(messages))
	for i, m := range messages {
		redacted[i] = redactMessageSecrets(m, secretCalls)
	}
	return redacted
}

func extractSecretCallMap(messages []types.Message) map[string]string {
	secretCalls := make(map[string]string)
	for _, m := range messages {
		for _, p := range m.Parts {
			partMap, ok := p.(map[string]any)
			if !ok || partMap["type"] != "tool_use" {
				continue
			}
			name, _ := partMap["name"].(string)
			if name != "secret_get" {
				continue
			}
			callID, _ := partMap["id"].(string)
			secretName := ""
			if inputMap, ok := partMap["input"].(map[string]any); ok {
				secretName, _ = inputMap["name"].(string)
			}
			if secretName == "" {
				secretName = "unknown"
			}
			if callID != "" {
				secretCalls[callID] = secretName
			}
		}
	}
	return secretCalls
}

func redactMessageSecrets(m types.Message, secretCalls map[string]string) types.Message {
	if len(m.Parts) == 0 {
		return m
	}
	msgCopy := m
	newParts := make([]any, len(m.Parts))
	for pi, p := range m.Parts {
		if partMap, ok := p.(map[string]any); ok && partMap["type"] == "tool_result" {
			toolCallID, _ := partMap["tool_use_id"].(string)
			if secName, exists := secretCalls[toolCallID]; exists {
				repText := fmt.Sprintf("[secret:%s redacted]", secName)
				newPart := make(map[string]any, len(partMap))
				for k, v := range partMap {
					newPart[k] = v
				}
				newPart["content"] = repText
				newParts[pi] = newPart
				msgCopy.Content = repText
				continue
			}
		}
		newParts[pi] = p
	}
	msgCopy.Parts = newParts
	return msgCopy
}

// ResumeAgenticLoop 在恢复时根据消息历史与元数据续跑 agentic 回合（R2-05）。
func (a *Agent) ResumeAgenticLoop(turnID string, messages []types.Message, taintLevel types.TaintLevel, round int) bool {
	if turnID == "" || len(messages) == 0 {
		return false
	}
	// 纵深防御：无论调用方是否已修复，续跑前再确保历史里没有悬空的 tool_calls。
	messages = RepairDanglingToolCalls(messages)
	a.sCtx.Mu.Lock()
	a.sCtx.TurnID = turnID
	a.sCtx.LoopRound = round
	a.sCtx.LoopMessages = messages
	if taintLevel > a.sCtx.GlobalTaintLevel {
		a.sCtx.GlobalTaintLevel = taintLevel
	}
	a.sCtx.Mu.Unlock()

	a.sm.ForceState(types.AgentStateLoop)
	a.asyncIntent(types.TriggerResume)
	return true
}

// interruptedToolResultText 回填给被中断工具调用的结果文本。
const interruptedToolResultText = "执行中断，结果未知，请自行检查后决定是否重试"

// CompleteInterruptedToolCalls 是 RepairDanglingToolCalls 的兼容入口（R2-05）。
func CompleteInterruptedToolCalls(messages []types.Message) []types.Message {
	return RepairDanglingToolCalls(messages)
}

// RepairDanglingToolCalls 修复崩溃恢复重放历史中“悬空”的工具调用：
//   - 任一 assistant 消息里的 tool_use 若没有紧随其后的 tool_result，就在该 assistant 的结果段末尾补一条
//     “执行中断”结果（而不是只修最后一条、也不是追加到整个序列末尾——后面若已有 steer/user 消息，
//     追加到末尾仍会让 tool 消息不紧跟 tool_calls）；
//   - 找不到对应 tool_use 的孤儿 tool_result 降级为普通 user 文本（保留信息，去掉协议非法的 tool 块）。
//
// DeepSeek/OpenAI 兼容协议要求 assistant.tool_calls 之后必须紧跟与每个 id 对应的 tool 消息，
// 否则整个请求返回 400（"insufficient tool messages following tool_calls message"）。
func RepairDanglingToolCalls(messages []types.Message) []types.Message {
	if len(messages) == 0 {
		return messages
	}
	out := make([]types.Message, 0, len(messages)+2)
	for i := 0; i < len(messages); i++ {
		m := messages[i]
		if m.Role != "assistant" {
			if isToolResultMessage(m) {
				// 孤儿 tool_result：前面没有声明它的 assistant.tool_use。协议上不能保留 tool 块，
				// 但其文本（错误回填、委派结果等）仍有信息价值，降级为普通 user 文本。
				if m.Content != "" {
					m.Role, m.Parts = "user", nil
					out = append(out, m)
				}
				continue
			}
			out = append(out, m)
			continue
		}
		out = append(out, m)
		ids := extractToolUseIDs(m.Parts)
		if len(ids) == 0 {
			continue
		}
		declared := make(map[string]bool, len(ids))
		for _, id := range ids {
			declared[id] = true
		}
		have := make(map[string]bool, len(ids))
		j := i + 1
		for ; j < len(messages) && isToolResultMessage(messages[j]); j++ {
			rids := extractToolResultIDs(messages[j : j+1])
			keep := false
			for id := range rids {
				if declared[id] {
					keep = true
					have[id] = true
				}
			}
			if keep {
				out = append(out, messages[j])
			}
		}
		for _, id := range ids {
			if !have[id] {
				synth := makeToolResultMessage(id, interruptedToolResultText)
				synth.Round = m.Round
				out = append(out, synth)
			}
		}
		i = j - 1
	}
	return out
}

func extractToolUseIDs(parts []any) []string {
	var callIDs []string
	for _, p := range parts {
		if partMap, ok := p.(map[string]any); ok && partMap["type"] == "tool_use" {
			if id, ok := partMap["id"].(string); ok && id != "" {
				callIDs = append(callIDs, id)
			}
		}
	}
	return callIDs
}

func extractToolResultIDs(subMessages []types.Message) map[string]bool {
	existingResults := make(map[string]bool)
	for _, m := range subMessages {
		for _, p := range m.Parts {
			if partMap, ok := p.(map[string]any); ok && partMap["type"] == "tool_result" {
				if callID, ok := partMap["tool_use_id"].(string); ok {
					existingResults[callID] = true
				}
			}
		}
	}
	return existingResults
}

// ReconstructAgenticLoopTurn 从 agent_loop_rounds 表中读取指定 running turn 的所有轮次并还原消息序列（R2-05）。
func ReconstructAgenticLoopTurn(ctx context.Context, q protocol.SQLQuerier, sessionID, turnID string) ([]types.Message, types.TaintLevel, int, error) {
	query := `SELECT round, delta_json, taint_level FROM agent_loop_rounds WHERE session_id = ? AND turn_id = ? AND status = 'running' ORDER BY round ASC`
	rows, err := q.QueryContext(ctx, query, sessionID, turnID)
	if err != nil {
		return nil, types.TaintNone, 0, apperr.Wrap(apperr.CodeInternal, "reconstruct turn: query agent_loop_rounds", err)
	}
	defer rows.Close()

	var allMessages []types.Message
	var maxRound int
	var maxTaint types.TaintLevel

	for rows.Next() {
		var round int
		var deltaJSON string
		var taintInt int
		if err := rows.Scan(&round, &deltaJSON, &taintInt); err != nil {
			return nil, types.TaintNone, 0, apperr.Wrap(apperr.CodeInternal, "reconstruct turn: scan round", err)
		}
		if round > maxRound {
			maxRound = round
		}
		if types.TaintLevel(taintInt) > maxTaint {
			maxTaint = types.TaintLevel(taintInt)
		}
		var delta []types.Message
		if err := json.Unmarshal([]byte(deltaJSON), &delta); err != nil {
			return nil, types.TaintNone, 0, apperr.Wrap(apperr.CodeInternal, "reconstruct turn: unmarshal delta", err)
		}
		allMessages = append(allMessages, delta...)
	}
	if err := rows.Err(); err != nil {
		return nil, types.TaintNone, 0, apperr.Wrap(apperr.CodeInternal, "reconstruct turn: rows iteration", err)
	}
	if len(allMessages) == 0 {
		return nil, types.TaintNone, 0, apperr.New(apperr.CodeNotFound, fmt.Sprintf("no rounds found for running turn %s", turnID))
	}

	allMessages = CompleteInterruptedToolCalls(allMessages)
	return allMessages, maxTaint, maxRound, nil
}
