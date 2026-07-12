package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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

func (w *storeEventWriter) writeEvent(sessionID string, evType string, payload map[string]any) {
	if w.store == nil || sessionID == "" {
		return
	}
	ts := time.Now().UnixNano()
	key := fmt.Sprintf("events:session:%s:%d", sessionID, ts)

	payload["type"] = evType
	payload["ts"] = ts

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
