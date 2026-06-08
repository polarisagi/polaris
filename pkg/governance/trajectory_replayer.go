package governance

import (
	"bufio"
	"encoding/json"
	"os"

	perrors "github.com/polarisagi/polaris/internal/errors"
)

// ErrReplayExhausted 表示轨迹回放耗尽。
var ErrReplayExhausted = perrors.New(perrors.CodeInternal, "trajectory replay exhausted")

// ErrDivergentTrajectory 表示轨迹偏离（当前系统行为与录制历史不一致）。
var ErrDivergentTrajectory = perrors.New(perrors.CodeInternal, "divergent trajectory detected")

// TrajectoryReplayer 支持从 JSONL 加载轨迹，无缝拦截底层的网络与工具调用，
// 达到零费用（zero-cost）与强确定性的评估目标。
type TrajectoryReplayer struct {
	events      []TrajectoryEvent
	replayIndex int
}

func NewTrajectoryReplayer() *TrajectoryReplayer {
	return &TrajectoryReplayer{
		events:      make([]TrajectoryEvent, 0),
		replayIndex: 0,
	}
}

// LoadTrajectory 从 JSONL 文件中逐行加载轨迹。
func (r *TrajectoryReplayer) LoadTrajectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var events []TrajectoryEvent
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev TrajectoryEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return perrors.Wrap(perrors.CodeInternal, "failed to parse event line", err)
		}
		events = append(events, ev)
	}

	if err := scanner.Err(); err != nil {
		return perrors.Wrap(perrors.CodeInternal, "error reading trajectory file", err)
	}

	r.events = events
	r.replayIndex = 0
	return nil
}

// InterceptLLMRequest 检查当前是否有一个预期的 LLM 响应。
// 比较请求（可选），如果不匹配则抛出偏离异常。
// 成功时将返回预录制的响应内容 JSON 字节。
func (r *TrajectoryReplayer) InterceptLLMRequest(requestData any) ([]byte, error) {
	for r.replayIndex < len(r.events) {
		ev := r.events[r.replayIndex]
		r.replayIndex++

		if ev.Type == "llm_request" {
			// 在实际实现中，这里可以比较请求 JSON 是否与预期一致
			// 如果差别巨大，可返回 ErrDivergentTrajectory
			continue
		} else if ev.Type == "llm_response" {
			return ev.Data, nil
		}
	}
	return nil, ErrReplayExhausted
}

// InterceptToolCall 类似地拦截工具执行，直接返回当时的结果。
func (r *TrajectoryReplayer) InterceptToolCall(toolCallData any) ([]byte, error) {
	for r.replayIndex < len(r.events) {
		ev := r.events[r.replayIndex]
		r.replayIndex++

		if ev.Type == "tool_call" {
			continue
		} else if ev.Type == "tool_result" {
			return ev.Data, nil
		}
	}
	return nil, ErrReplayExhausted
}

// SetEvents 直接注入事件列表，用于测试和内存回放（不依赖文件系统）。
func (r *TrajectoryReplayer) SetEvents(events []TrajectoryEvent) {
	r.events = events
	r.replayIndex = 0
}

// IsExhausted 判断是否已经回放完毕所有事件
func (r *TrajectoryReplayer) IsExhausted() bool {
	return r.replayIndex >= len(r.events)
}
