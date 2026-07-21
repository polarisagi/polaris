package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
)

// storeEventWriter implements fsm.SessionEventWriter and tool.SessionEventWriter
type storeEventWriter struct {
	store protocol.Store
}

func newStoreEventWriter(store protocol.Store) *storeEventWriter {
	return &storeEventWriter{store: store}
}

// eventSeqTiebreaker 单调递增计数器，作为同一纳秒内多个事件的排序/唯一性
// 兜底（2026-07-22 崩溃恢复回放接线补齐）。原 key 仅用 time.Now().UnixNano()，
// 理论上同一 session 在极短时间内连续写入两条事件时纳秒值可能相同，导致后写
// 覆盖前写、静默丢事件——对 §8 崩溃恢复"必须拿到完整、有序的 LLM 调用记录才能
// 回放"这一新需求是真实风险（对纯观测用途的历史读取影响不大，但回放要求
// 一条不漏）。不依赖持久化跨重启的单调性——每次进程重启计数器归零，但同一
// 进程内 time.Now().UnixNano() 严格新于重启前的所有历史值（系统时钟不回拨的
// 前提下），故跨重启的整体有序性仍成立，只补齐同一进程内同纳秒的排序问题。
//
//nolint:gochecknoglobals // atomic 计数器，非可变共享状态语义（并发安全单调递增，无需实例化）
var eventSeqTiebreaker atomic.Uint64

func (w *storeEventWriter) writeEvent(sessionID string, evType string, payload map[string]any) {
	if w.store == nil || sessionID == "" {
		return
	}
	ts := time.Now().UnixNano()
	seq := eventSeqTiebreaker.Add(1)
	key := fmt.Sprintf("events:session:%s:%020d_%020d", sessionID, ts, seq)

	payload["type"] = evType
	payload["ts"] = ts
	payload["seq"] = seq

	val, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("storeEventWriter: failed to marshal event", "type", evType, "err", err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := w.store.Put(ctx, []byte(key), val); err != nil {
		slog.Warn("storeEventWriter: failed to put event", "type", evType, "err", err)
	}
}

func (w *storeEventWriter) WriteStateTransEvent(sessionID string, stateType string) {
	w.writeEvent(sessionID, stateType, map[string]any{})
}

func (w *storeEventWriter) WriteLLMCallEvent(sessionID string, request, response map[string]any) {
	w.writeEvent(sessionID, "llm_call", map[string]any{
		"request":  request,
		"response": response,
	})
}

func (w *storeEventWriter) WriteToolCallEvent(sessionID, toolName string, input, output map[string]any) {
	w.writeEvent(sessionID, "tool_call", map[string]any{
		"tool":   toolName,
		"args":   input,
		"result": output,
	})
}
