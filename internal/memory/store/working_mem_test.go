package store

import (
	"context"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/memory/testutil"
)

func TestWorkingMemDB(t *testing.T) {
	w := NewWorkingMemWithDB(nil)
	if w.Notes() == nil {
		t.Fatal("Notes should be initialized")
	}
}

func TestImmutableCore(t *testing.T) {
	ic := NewImmutableCore()
	ic.GlobalGoal = "goal"
	ic.UserPreferences["theme"] = "dark"

	view, err := ic.Load(context.Background(), "user", "session")
	if err != nil {
		t.Fatal(err)
	}
	if view.SessionGoal != "goal" {
		t.Fatal("goal mismatch")
	}

	ic.SystemPromptTemplate = "Hello {{.GlobalGoal}}"
	res := ic.renderSystemPrompt()
	if res != "Hello goal" {
		t.Fatalf("template failed: %s", res)
	}

	ic.SystemPromptTemplate = "{{.Invalid}}"
	res = ic.renderSystemPrompt()
	if len(res) == 0 {
		t.Fatal("expected error message")
	}

	ic.SystemPromptTemplate = ""
	ic.SoulMDContent = "soul"
	ic.ModelGuidance = "guide"
	ic.CustomInstructions = "custom"
	ic.PlatformHint = "hint"
	ic.VolatileBlock = "volatile"
	res = ic.renderSystemPrompt()
	if res != "soul\n\nguide\n\ncustom\n\nhint\n\nvolatile" {
		t.Fatalf("parts assembly failed: %s", res)
	}

	msgs := []types.Message{{Role: "user", Content: "hi"}}
	msgs = ic.PrependToMessages(msgs)
	if len(msgs) != 2 || msgs[0].Role != "system" {
		t.Fatal("PrependToMessages failed")
	}

	// Default fallback
	ic.SoulMDContent = ""
	ic.ModelGuidance = ""
	ic.CustomInstructions = ""
	ic.PlatformHint = ""
	ic.VolatileBlock = ""
	msgs = ic.PrependToMessages([]types.Message{})
	if msgs[0].Content == "" {
		t.Fatal("default content failed")
	}
}

func TestContextWindowCompress(t *testing.T) {
	cw := NewContextWindow(5)
	cw.Append(types.Message{Role: "system", Content: "sys"})
	cw.Append(types.Message{Role: "tool", Content: "tool1"})
	cw.Append(types.Message{Role: "user", Content: "user1"})
	cw.Append(types.Message{Role: "assistant", Content: "ast1"})
	cw.Append(types.Message{Role: "user", Content: "user2"})
	cw.Append(types.Message{Role: "user", Content: "user3"}) // capacity exceeded

	// Should have evicted tool1 or similar
	if cw.Tokens() == 0 {
		t.Fatal("Tokens failed")
	}

	ctx := context.Background()
	// Compress to very small token count
	err := cw.Compress(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}

	em := NewEpisodicMem(testutil.NewMockStore())

	cw2 := NewContextWindow(10)
	cw2.Append(types.Message{Role: "system", Content: "sys"})
	cw2.Append(types.Message{Role: "tool", Content: "t1"})
	cw2.Append(types.Message{Role: "user", Content: "u1"})

	err = CompactWorkingMemory(ctx, cw2, em, 5)
	if err != nil {
		t.Fatal(err)
	}
}

func TestScratchPad(t *testing.T) {
	s := NewScratchPad()
	s.Set("key", "val")
	v, ok := s.Get("key")
	if !ok || v != "val" {
		t.Fatal("ScratchPad failed")
	}
	s.Clear()
	_, ok = s.Get("key")
	if ok {
		t.Fatal("Clear failed")
	}
}
