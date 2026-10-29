package fsm

import "github.com/polarisagi/polaris/pkg/types"

// LoopStream 记录 agentic 回合已流式推给用户的文本（R5-02）。
// 单独成体是为了不撑大 state_machine.go（R7 行数门控），并随 ResetAgenticTurn 整体清零。
type LoopStream struct {
	// LoopReplyStreamed 本回合是否已有文本增量以 Token 事件推给用户；
	// 为真时终态不得再用 publishPreparedReply 重发一遍。
	LoopReplyStreamed bool
	// LoopStreamedText 各轮已推文本按序拼接，等于用户实际看到的回复，
	// 也是写入 ConversationHistory 的助手内容。
	LoopStreamedText string
	// LoopEmptyRetries 当前这次「既无正文也无工具调用」空输出事件内已追加的额外 LLM 调用次数
	// （R5-06；上限 loopEmptyMaxExtraCalls）。任一轮拿到非空输出即清零，故按事件而非整回合计。
	LoopEmptyRetries int
	// LoopLastReasoning 最近一轮 LLM 响应的思考链（可为空）。空输出重试时回传给 DeepSeek，
	// 且每轮覆盖写，避免用到更早轮次的陈旧思考。
	LoopLastReasoning string
}

// loopEmptyMaxExtraCalls 单次空输出事件允许的额外 LLM 调用上限（B3.3）：
// 第 1 次=提示重试；第 2 次=有工具结果则无工具收尾、否则关闭思考重试。
const loopEmptyMaxExtraCalls = 2

const (
	// loopEmptyNudge 空输出重试时追加的 user 提示（B3.1）：重发完全相同的请求大概率得到同样的空结果。
	loopEmptyNudge = "上一轮只完成了思考，没有发出工具调用或回答。请直接发出工具调用，或给出最终回答。"
	// loopEmptyWrapUpNudge 无工具收尾调用的提示（B3.2）。
	loopEmptyWrapUpNudge = "上一轮仍未给出回答。不要再调用工具，请基于已获得的工具结果直接给出最终回答。"
	// loopEmptyAssistantPlaceholder 占位助手消息正文：DeepSeek 要求回传该轮 reasoning_content，
	// 而空正文且无 tool_calls 的 assistant 消息会被部分兼容端点拒收。
	loopEmptyAssistantPlaceholder = "（上一轮只完成了思考，没有输出正文或工具调用。）"
)

// LoopEmptyRecovery 描述空输出恢复链当前所处档位，由 Agent 在发起 LLM 调用时读取。
type LoopEmptyRecovery struct {
	Attempt         int  // 0=正常；1/2=第几次额外调用
	ForbidTools     bool // 本次调用 tool_choice=none（已有工具结果，强制收尾）
	DisableThinking bool // 本次调用关闭思考模式（尚无工具结果的最后一搏）
}

// LoopEmptyRecoveryPlan 返回当前空输出恢复档位。
func (s *StateContext) LoopEmptyRecoveryPlan() LoopEmptyRecovery {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	return s.loopEmptyRecoveryLocked()
}

func (s *StateContext) loopEmptyRecoveryLocked() LoopEmptyRecovery {
	r := LoopEmptyRecovery{Attempt: s.LoopEmptyRetries}
	if s.LoopEmptyRetries < 2 {
		return r
	}
	if loopHasToolResults(s.LoopMessages) {
		r.ForbidTools = true
	} else {
		r.DisableThinking = true
	}
	return r
}

// loopHasToolResults 本回合是否已有工具结果（tool_result 块）。
func loopHasToolResults(msgs []types.Message) bool {
	for _, m := range msgs {
		for _, p := range m.Parts {
			if pm, ok := p.(map[string]any); ok && pm["type"] == "tool_result" {
				return true
			}
		}
	}
	return false
}

// loopEmptyRetryMessages 空输出重试请求在 L4 末尾追加的瞬态消息（不写入 LoopMessages，
// 以免污染历史、持久化快照与「最后一段助手文本」判定）。无空输出事件时返回 nil。
func (s *StateContext) loopEmptyRetryMessages(round int) []types.Message {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	if s.LoopEmptyRetries == 0 {
		return nil
	}
	nudge := loopEmptyNudge
	if s.loopEmptyRecoveryLocked().ForbidTools {
		nudge = loopEmptyWrapUpNudge
	}
	return []types.Message{
		{Role: "assistant", Content: loopEmptyAssistantPlaceholder, ReasoningContent: s.LoopLastReasoning, Round: round},
		{Role: "user", Content: nudge, Round: round},
	}
}

// TurnOrigin 回合来源标记。不属于 AgenticTurn：它由 Pool 在获取实例时写入，
// 不能被 ResetAgenticTurn 清掉。
type TurnOrigin struct {
	// BackgroundTurn 本轮经 AcquireHeadless 获取（cron / workflow / 委派等无人阅读的回合）。
	BackgroundTurn bool
}

// SetBackgroundTurn 记录回合来源。
func (s *StateContext) SetBackgroundTurn(background bool) {
	s.Mu.Lock()
	s.BackgroundTurn = background
	s.Mu.Unlock()
}

// LoopStreamsToUser 判定本回合的循环 LLM 调用是否面向用户流式输出：
// 交互回合（完成模式非 explicit 且非后台）才有读者；委派子智能体用 explicit，
// cron / workflow 走后台获取，二者保持「回合末一次性发布」的现状。
func (s *StateContext) LoopStreamsToUser() bool {
	s.Mu.RLock()
	defer s.Mu.RUnlock()
	return s.LoopCompletion != "explicit" && !s.BackgroundTurn
}

// RecordLoopStreamedLocked 记录一轮已流式推送的文本。调用方须持有 s.Mu 写锁。
func (s *StateContext) RecordLoopStreamedLocked(chunk string) {
	if chunk == "" {
		return
	}
	s.LoopReplyStreamed = true
	s.LoopStreamedText += chunk
}
