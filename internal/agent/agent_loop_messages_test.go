package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

// TestGate_SingleAppendLoopMessagesEntry 门控测试（R3-06 / R4-06）：
// 递归扫描 internal/agent/... 所有 Go 代码，断言除 fsm/state_machine.go 外，任何生产文件不得出现 LoopMessages = append。
func TestGate_SingleAppendLoopMessagesEntry(t *testing.T) {
	root := "."
	if _, err := os.Stat("agent_loop_messages.go"); err != nil {
		// 若当前在仓库根目录运行测试
		root = "internal/agent"
	}

	var scanned int
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// 允许测试文件中的测试数据装配，生产代码严格拦截
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// 只允许 fsm/state_machine.go (AppendLoopMessages / ResetAgenticTurn) 和 agent_loop_checkpoint.go (ResumeAgenticLoop) 改写该字段
		base := filepath.Base(path)
		if base == "state_machine.go" || base == "agent_loop_checkpoint.go" {
			return nil
		}

		scanned++
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines := strings.Split(string(content), "\n")
		for lineIdx, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(line, "LoopMessages = append") {
				t.Errorf("%s:%d contains 'LoopMessages = append'; all message additions must go through sCtx.AppendLoopMessages", path, lineIdx+1)
			}
			if strings.Contains(line, "LoopMessages =") {
				t.Errorf("%s:%d directly mutates LoopMessages; all message modifications must go through ResetAgenticTurn or AppendLoopMessages", path, lineIdx+1)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk %s failed: %v", root, err)
	}
	if scanned < 10 {
		t.Fatalf("expected >= 10 production go files scanned under %s, got %d", root, scanned)
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
