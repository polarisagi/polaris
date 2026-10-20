package fsm

import (
	"context"
	"log/slog"

	"github.com/polarisagi/polaris/internal/prompt"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/types"
)

func getLastAssistantText(sCtx *StateContext, fill []byte) string {
	if len(fill) > 0 {
		return string(fill)
	}
	if len(sCtx.LoopMessages) > 0 {
		lastMsg := sCtx.LoopMessages[len(sCtx.LoopMessages)-1]
		if lastMsg.Role == "assistant" {
			return lastMsg.Content
		}
	}
	return ""
}

func formatUsageLine(tokens int, cost float64, wallSeconds float64) string {
	return "[usage] tokens=" + itoa(tokens) + " cost≈$" + formatCost(cost) + " wall=" + itoa(int(wallSeconds)) + "s"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	bp := len(b)
	neg := i < 0
	u := uint(i)
	if neg {
		u = uint(-i)
	}
	for u > 0 {
		bp--
		b[bp] = byte('0' + u%10)
		u /= 10
	}
	if neg {
		bp--
		b[bp] = '-'
	}
	return string(b[bp:])
}

func formatCost(c float64) string {
	if c <= 0 {
		return "0.0000"
	}
	cents := int(c * 10000)
	intPart := cents / 10000
	fracPart := cents % 10000
	fracStr := itoa(fracPart)
	for len(fracStr) < 4 {
		fracStr = "0" + fracStr
	}
	return itoa(intPart) + "." + fracStr
}

func fallbackAgenticPrompt(ctx context.Context, mem protocol.MemoryFacade, sCtx *StateContext, roundMsgs []types.Message) []types.Message {
	b := prompt.NewPromptBuilder()
	if wErr := writeFrozenSessionLayer(ctx, b, mem, sCtx); wErr != nil {
		slog.WarnContext(ctx, "agentic_loop: WriteSessionLayer fallback failed", "err", wErr)
	}
	WriteConversationHistory(b, sCtx)
	sCtx.Mu.RLock()
	warnPrompt := sCtx.LoopWarningPrompt
	sCtx.Mu.RUnlock()

	if len(roundMsgs) == 0 {
		sCtx.Mu.RLock()
		intent := sCtx.RawIntentTS
		sCtx.Mu.RUnlock()
		if !intent.IsEmpty() {
			b.WriteUserData(intent)
		}
		return FinishAgenticLayered(b, mem, loopInstruction(), "", warnPrompt)
	}
	for _, m := range roundMsgs {
		b.WriteTurnMessage(m)
	}
	return FinishAgenticLayered(b, mem, loopInstruction(), "", warnPrompt)
}
