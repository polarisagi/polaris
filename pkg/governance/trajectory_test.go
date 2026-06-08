package governance

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestTrajectoryRecordAndReplay(t *testing.T) {
	recorder := NewTrajectoryRecorder()

	// 1. 录制过程
	reqData := map[string]string{"prompt": "Hello"}
	respData := map[string]string{"reply": "World"}
	toolCall := map[string]string{"tool": "weather"}
	toolRes := map[string]string{"temp": "25C"}

	recorder.RecordLLMRequest(reqData)
	recorder.RecordLLMResponse(respData)
	recorder.RecordToolCall(toolCall)
	recorder.RecordToolResult(toolRes)

	events := recorder.GetEvents()
	if len(events) != 4 {
		t.Fatalf("expected 4 events, got %d", len(events))
	}

	// 2. 落盘
	dir, err := os.MkdirTemp("", "polaris_eval_test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "traj.jsonl")
	if err := recorder.Save(path); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	// 3. 回放过程
	replayer := NewTrajectoryReplayer()
	if err := replayer.LoadTrajectory(path); err != nil {
		t.Fatalf("load failed: %v", err)
	}

	// 预期拦截 LLM，获得响应
	b, err := replayer.InterceptLLMRequest(reqData)
	if err != nil {
		t.Fatalf("llm intercept failed: %v", err)
	}
	var gotResp map[string]string
	json.Unmarshal(b, &gotResp)
	if gotResp["reply"] != "World" {
		t.Errorf("expected reply 'World', got %s", gotResp["reply"])
	}

	// 预期拦截 Tool，获得结果
	tb, err := replayer.InterceptToolCall(toolCall)
	if err != nil {
		t.Fatalf("tool intercept failed: %v", err)
	}
	var gotTool map[string]string
	json.Unmarshal(tb, &gotTool)
	if gotTool["temp"] != "25C" {
		t.Errorf("expected tool result '25C', got %s", gotTool["temp"])
	}

	if !replayer.IsExhausted() {
		t.Errorf("replayer should be exhausted")
	}

	// 预期 Exhausted 错误
	if _, err := replayer.InterceptLLMRequest(nil); err != ErrReplayExhausted {
		t.Errorf("expected ErrReplayExhausted, got %v", err)
	}
}
