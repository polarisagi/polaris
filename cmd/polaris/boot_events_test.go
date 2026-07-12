package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

type mockStore struct {
	protocol.Store
	putKeys   [][]byte
	putValues [][]byte
}

func (m *mockStore) Put(ctx context.Context, key []byte, val []byte) error {
	m.putKeys = append(m.putKeys, key)
	m.putValues = append(m.putValues, val)
	return nil
}

func TestStoreEventWriter_WriteStateTransEvent(t *testing.T) {
	ms := &mockStore{}
	w := newStoreEventWriter(ms)

	w.WriteStateTransEvent("sess-1", "S_PLAN")

	if len(ms.putKeys) != 1 {
		t.Fatalf("expected 1 put, got %d", len(ms.putKeys))
	}

	keyStr := string(ms.putKeys[0])
	if keyStr[:15] != "events:session:" {
		t.Errorf("key prefix mismatch: %s", keyStr)
	}

	var val map[string]any
	if err := json.Unmarshal(ms.putValues[0], &val); err != nil {
		t.Fatal(err)
	}
	if val["type"] != "S_PLAN" {
		t.Errorf("expected type S_PLAN, got %v", val["type"])
	}
}

func TestStoreEventWriter_WriteLLMCallEvent(t *testing.T) {
	ms := &mockStore{}
	w := newStoreEventWriter(ms)

	req := map[string]any{"prompt": "hello"}
	resp := map[string]any{"output": "world"}
	w.WriteLLMCallEvent("sess-1", req, resp)

	if len(ms.putKeys) != 1 {
		t.Fatalf("expected 1 put, got %d", len(ms.putKeys))
	}

	var val map[string]any
	json.Unmarshal(ms.putValues[0], &val)
	if val["type"] != "llm_call" {
		t.Errorf("expected type llm_call, got %v", val["type"])
	}
}

func TestStoreEventWriter_WriteToolCallEvent(t *testing.T) {
	ms := &mockStore{}
	w := newStoreEventWriter(ms)

	req := map[string]any{"arg1": "a"}
	resp := map[string]any{"res1": "b"}
	w.WriteToolCallEvent("sess-1", "read_file", req, resp)

	if len(ms.putKeys) != 1 {
		t.Fatalf("expected 1 put, got %d", len(ms.putKeys))
	}

	var val map[string]any
	json.Unmarshal(ms.putValues[0], &val)
	if val["type"] != "tool_call" {
		t.Errorf("expected type tool_call, got %v", val["type"])
	}
}
