package session

import (
	"strings"
	"sync"
)

// BufferSink 缓冲式 Sink 实现：累积 KindDelta 增量文本，忽略状态类事件的
// 展示（KindStatus/KindContextWarning/KindToolCall），供非交互式调用方
// （Headless：Cron/Workflow/Webhook）使用——RunTurn 内部仍是同一套逐事件
// 驱动循环，调用方只关心最终聚合结果（Request.Streaming=false 语义）。
//
// Emit 语义上单 goroutine 串行调用（RunTurn 内部事件循环非并发），加锁仅为
// 防御调用方误用导致的数据竞争，不构成性能热点。
type BufferSink struct {
	mu      sync.Mutex
	reply   strings.Builder
	lastErr error
}

// NewBufferSink 构造一个空的 BufferSink。
func NewBufferSink() *BufferSink {
	return &BufferSink{}
}

// Emit 实现 Sink 接口。BufferSink 永不因下游不可写而返回 error（纯内存
// 累积，无 IO），始终返回 nil。
func (s *BufferSink) Emit(ev Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch ev.Kind {
	case KindDelta:
		s.reply.WriteString(ev.Text)
	case KindError:
		if ev.Err != nil {
			s.lastErr = ev.Err
		}
	}
	return nil
}

// String 返回当前累积的完整回复文本。
func (s *BufferSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reply.String()
}

// LastError 返回最近一次 KindError 事件携带的 error（无错误时为 nil）。
func (s *BufferSink) LastError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}
