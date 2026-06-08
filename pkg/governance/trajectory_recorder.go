package governance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// TrajectoryRecorder 记录运行时的 LLM 调用、Tool 执行、状态变更等事件，
// 以 JSONL 格式序列化保存，支持未来的零费用确定性回放。
// 架构文档: docs/arch/12-Eval-Harness-深度选型.md §3
type TrajectoryRecorder struct {
	mu     sync.Mutex
	events []TrajectoryEvent
	seqSeq int
}

func NewTrajectoryRecorder() *TrajectoryRecorder {
	return &TrajectoryRecorder{
		events: make([]TrajectoryEvent, 0),
	}
}

// RecordLLMRequest 记录向 LLM 发出的请求。
func (r *TrajectoryRecorder) RecordLLMRequest(data any) {
	r.recordEvent("llm_request", data)
}

// RecordLLMResponse 记录 LLM 返回的响应。
func (r *TrajectoryRecorder) RecordLLMResponse(data any) {
	r.recordEvent("llm_response", data)
}

// RecordToolCall 记录工具调用。
func (r *TrajectoryRecorder) RecordToolCall(data any) {
	r.recordEvent("tool_call", data)
}

// RecordToolResult 记录工具执行结果。
func (r *TrajectoryRecorder) RecordToolResult(data any) {
	r.recordEvent("tool_result", data)
}

func (r *TrajectoryRecorder) recordEvent(eventType string, data any) {
	b, _ := json.Marshal(data)
	r.mu.Lock()
	defer r.mu.Unlock()

	r.seqSeq++
	event := TrajectoryEvent{
		Seq:       r.seqSeq,
		Timestamp: time.Now().UnixNano(),
		Type:      eventType,
		Data:      b,
	}
	r.events = append(r.events, event)
}

// Save 将录制的轨迹落盘（以 JSONL 格式）。
// path 通常位于 ~/.polarisagi/polaris/eval/training/ 或类似目录，受到 M11 保护。
func (r *TrajectoryRecorder) Save(path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// 确保目录存在
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "failed to create directory", err)
	}

	f, err := os.Create(path)
	if err != nil {
		return perrors.Wrap(perrors.CodeInternal, "failed to create trajectory file", err)
	}
	defer f.Close()

	for _, ev := range r.events {
		b, err := json.Marshal(ev)
		if err != nil {
			return perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("failed to marshal event %d", ev.Seq), err)
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			return perrors.Wrap(perrors.CodeInternal, fmt.Sprintf("failed to write event %d", ev.Seq), err)
		}
	}

	return nil
}

// GetEvents 返回当前所有事件的一个快照。
func (r *TrajectoryRecorder) GetEvents() []TrajectoryEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	res := make([]TrajectoryEvent, len(r.events))
	copy(res, r.events)
	return res
}
