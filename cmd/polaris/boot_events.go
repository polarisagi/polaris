package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/protocol/repo"
)

// trajectoryAppender storeEventWriter 对轨迹账本的消费端接口（HE-3）；
// 由 store/repo.SQLiteTrajectoryRepository 实现（写连接）。
type trajectoryAppender interface {
	Append(ctx context.Context, sessionID, eventType, toolName string, toolOK *bool, latencyMs int64, payload string) (int64, error)
}

// storeEventWriter implements fsm.SessionEventWriter and tool.SessionEventWriter。
// 写 session_trajectory（045，ADR-0104 决策七）：seq 由库内单语句分配，
// 不再靠纳秒时间戳 + 进程级全局计数器排序。
type storeEventWriter struct {
	traj trajectoryAppender
}

func newStoreEventWriter(traj trajectoryAppender) *storeEventWriter {
	return &storeEventWriter{traj: traj}
}

// noLatency 非工具事件的耗时占位（落库为 NULL）。
const noLatency int64 = -1

func (w *storeEventWriter) writeEvent(sessionID, evType, toolName string, toolOK *bool, latencyMs int64, payload map[string]any) {
	if w.traj == nil || sessionID == "" {
		return
	}
	val, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("storeEventWriter: failed to marshal event", "type", evType, "err", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := w.traj.Append(ctx, sessionID, evType, toolName, toolOK, latencyMs, string(val)); err != nil {
		slog.Warn("storeEventWriter: failed to append trajectory event", "type", evType, "err", err)
	}
}

func (w *storeEventWriter) WriteStateTransEvent(sessionID string, stateType string) {
	w.writeEvent(sessionID, stateType, "", nil, noLatency, map[string]any{})
}

func (w *storeEventWriter) WriteLLMCallEvent(sessionID string, request, response map[string]any) {
	w.writeEvent(sessionID, repo.TrajectoryEventLLMCall, "", nil, noLatency, map[string]any{
		"request":  request,
		"response": response,
	})
}

func (w *storeEventWriter) WriteToolCallEvent(sessionID, toolName string, input, output map[string]any, ok bool, latencyMs int64) {
	if latencyMs < 0 {
		latencyMs = 0
	}
	w.writeEvent(sessionID, repo.TrajectoryEventToolCall, toolName, &ok, latencyMs, map[string]any{
		"tool":   toolName,
		"args":   input,
		"result": output,
	})
}
