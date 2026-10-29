package fsm

import (
	"context"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

type indexedMem struct {
	protocol.MemoryFacade
	index string
}

func (m indexedMem) ListCoreMemory(context.Context, string, string) ([]types.CoreMemoryBlock, error) {
	return nil, nil
}

func (m indexedMem) SessionIndex(context.Context, string, string) (string, error) {
	return m.index, nil
}

func sessionText(t *testing.T, mem protocol.MemoryFacade) string {
	t.Helper()
	b := prompt.NewPromptBuilder()
	if err := WriteSessionLayer(t.Context(), b, mem, &StateContext{AgentID: "a", SessionID: "s"}); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, m := range b.LayerMessages(protocol.LayerSession) {
		sb.WriteString(m.Content)
	}
	return sb.String()
}

func TestWriteSessionLayer_IncludesMemoryIndex(t *testing.T) {
	got := sessionText(t, indexedMem{index: "long-term memory: 3 entities; recent: Polaris"})
	if !strings.Contains(got, "memory_index") || !strings.Contains(got, "3 entities") {
		t.Fatalf("会话层缺少记忆索引:\n%s", got)
	}
}

func TestWriteSessionLayer_NoIndexWhenEmptyOrUnsupported(t *testing.T) {
	if got := sessionText(t, indexedMem{}); strings.Contains(got, "memory_index") {
		t.Fatalf("空索引不应写入:\n%s", got)
	}
}
