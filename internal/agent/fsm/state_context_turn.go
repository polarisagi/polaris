package fsm

// NotePrefixBreakLocked 记录一次会使前缀缓存失效的事件；同一调用间隔内保留最先发生的一个。
// 调用方持有 s.Mu。
func (s *StateContext) NotePrefixBreakLocked(reason string) {
	if s.PendingPrefixBreak == "" {
		s.PendingPrefixBreak = reason
	}
}

// TakePrefixBreakLocked 取出并清空待归因的前缀断裂原因。调用方持有 s.Mu。
func (s *StateContext) TakePrefixBreakLocked() string {
	r := s.PendingPrefixBreak
	s.PendingPrefixBreak = ""
	return r
}
