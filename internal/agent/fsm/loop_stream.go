package fsm

// LoopStream 记录 agentic 回合已流式推给用户的文本（R5-02）。
// 单独成体是为了不撑大 state_machine.go（R7 行数门控），并随 ResetAgenticTurn 整体清零。
type LoopStream struct {
	// LoopReplyStreamed 本回合是否已有文本增量以 Token 事件推给用户；
	// 为真时终态不得再用 publishPreparedReply 重发一遍。
	LoopReplyStreamed bool
	// LoopStreamedText 各轮已推文本按序拼接，等于用户实际看到的回复，
	// 也是写入 ConversationHistory 的助手内容。
	LoopStreamedText string
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
func (s *StateContext) RecordLoopStreamedLocked(text string) {
	if text == "" {
		return
	}
	s.LoopReplyStreamed = true
	s.LoopStreamedText += text
}
