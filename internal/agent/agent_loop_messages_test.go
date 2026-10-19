package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

// TestGate_SingleAppendLoopMessagesEntry 门控测试：
// 扫描 internal/agent/*.go，断言除 agent_loop_messages.go 外，任何文件不得出现 LoopMessages = append (R3-06)。
func TestGate_SingleAppendLoopMessagesEntry(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("Glob *.go failed: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no go files found in internal/agent")
	}

	for _, f := range files {
		if f == "agent_loop_messages.go" || f == "agent_loop_messages_test.go" {
			continue
		}
		content, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("ReadFile %s failed: %v", f, err)
		}
		lines := strings.Split(string(content), "\n")
		for lineIdx, line := range lines {
			if strings.Contains(line, "LoopMessages = append") {
				t.Errorf("%s:%d contains 'LoopMessages = append'; all message additions must go through appendLoopMessages", f, lineIdx+1)
			}
		}
	}
}

// TestR3_06_MessagesRoundPreservedInReconstruction 验证 R3-06：
// 错误回填、插话注入、委派恢复三类消息均在持久化与崩溃重构后完整保留，且 Round 均为当前轮次。
func TestR3_06_MessagesRoundPreservedInReconstruction(t *testing.T) {
	db := newTestLoopRoundsDB(t)
	sessionID := "sess-r3-06"
	turnID := "turn-r3-06"

	agent := NewAgentWithDefaults(sessionID)
	agent.SetExtQuerier(db)
	agent.sCtx.SessionID = sessionID
	agent.sCtx.TurnID = turnID
	agent.sCtx.LoopRound = 2

	// a) 错误回填 (appendToolErrorMessages)
	errCalls := []types.InferToolCall{
		{ID: "err-call-1", Name: "bad_tool"},
	}
	agent.appendToolErrorMessages(errCalls, "permission denied")

	// b) 插话注入 (appendLoopMessages)
	agent.appendLoopMessages(types.Message{
		Role:    "user",
		Content: "user steer message",
	})

	// c) 委派恢复 (resumeHandoffInLoop)
	agent.sCtx.Mu.Lock()
	agent.sCtx.LoopHandoffSpawnCallID = map[string][]string{
		"call-spawn-1": {"child-1"},
	}
	agent.sCtx.LoopHandoffToolCalls = []types.InferToolCall{
		{ID: "call-spawn-1", Name: "spawn_agents"},
	}
	agent.sCtx.Mu.Unlock()

	if err := agent.resumeHandoffInLoop(context.Background()); err != nil {
		t.Fatalf("resumeHandoffInLoop failed: %v", err)
	}

	// 显式持久化增量
	agent.persistLoopRoundDelta(context.Background())

	// 通过 ReconstructAgenticLoopTurn 重建消息序列
	messages, _, maxRound, err := ReconstructAgenticLoopTurn(context.Background(), db, sessionID, turnID)
	if err != nil {
		t.Fatalf("ReconstructAgenticLoopTurn failed: %v", err)
	}

	if maxRound != 2 {
		t.Errorf("expected maxRound 2, got %d", maxRound)
	}

	// 断言这三类消息均在重建列表中完整保留，且 Round 均为当前轮次 (2)
	var foundError, foundSteer, foundHandoff bool
	for _, m := range messages {
		if strings.Contains(m.Content, "permission denied") {
			foundError = true
			if m.Round != 2 {
				t.Errorf("error message Round=%d, expected 2", m.Round)
			}
		}
		if strings.Contains(m.Content, "user steer message") {
			foundSteer = true
			if m.Round != 2 {
				t.Errorf("steer message Round=%d, expected 2", m.Round)
			}
		}
		if strings.Contains(m.Content, "handoff poster missing") || strings.Contains(m.Content, "child-1") {
			foundHandoff = true
			if m.Round != 2 {
				t.Errorf("handoff message Round=%d, expected 2", m.Round)
			}
		}
	}

	if !foundError {
		t.Error("expected error message to be reconstructed")
	}
	if !foundSteer {
		t.Error("expected steer message to be reconstructed")
	}
	if !foundHandoff {
		t.Error("expected handoff resume message to be reconstructed")
	}
}
