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

// formatUsageLine 调用级计量行（ADR-0120 决策八）：
// [usage] ctx=<当前估计>/<窗口> cache=<本回合命中率>% turn=$<本回合费用> session=$<会话费用> wall=<秒>s
// 位于请求末尾，不影响前缀；数值取整以保持短。
func formatUsageLine(ctxTokens, window, turnInput, turnCacheHit int, turnCost, sessionCost, wallSeconds float64) string {
	cache := "-"
	if turnInput > 0 {
		cache = itoa(turnCacheHit*100/turnInput) + "%"
	}
	return "[usage] ctx=" + formatK(ctxTokens) + "/" + formatK(window) + " cache=" + cache +
		" turn=$" + formatCost(turnCost) + " session=$" + formatCost(sessionCost) + " wall=" + itoa(int(wallSeconds)) + "s"
}

// formatK 把 token 数写成 "12.3k"；小于 1000 原样。
func formatK(n int) string {
	if n < 1000 {
		return itoa(n)
	}
	return itoa(n/1000) + "." + itoa(n%1000/100) + "k"
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
