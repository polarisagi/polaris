package agent

import (
	"context"

	"github.com/polarisagi/polaris/pkg/types"
)

// ContextAssembler 负责组装 LLM 的上下文 (CC-3: Context Assembler).
// 它统一收集系统提示词、短期记忆（Session Context）、工具执行结果、
// 长期记忆（语义记忆 / 情景记忆）和防护栏（TaintWarning 等）。
type ContextAssembler struct {
	memInjector MemoryInjector
}

// NewContextAssembler 创建一个新的 ContextAssembler。
func NewContextAssembler(memInjector MemoryInjector) *ContextAssembler {
	return &ContextAssembler{
		memInjector: memInjector,
	}
}

// Assemble 整合所有上下文信息生成供 LLM 消费的消息列表。
func (ca *ContextAssembler) Assemble(
	ctx context.Context,
	sessionID string,
	goal string,
	sysPrompt string,
	shortTermHistory []types.Message,
	taintWarning string,
) ([]types.Message, error) {
	var msgs []types.Message

	// 1. 系统提示词
	if sysPrompt != "" {
		msgs = append(msgs, types.Message{Role: "system", Content: sysPrompt})
	}

	// 2. Taint Warning (如 M05 的高污点警告)
	if taintWarning != "" {
		msgs = append(msgs, types.Message{Role: "system", Content: taintWarning})
	}

	// 3. 长期记忆注入
	if ca.memInjector != nil {
		memCtx, err := ca.memInjector.InjectRelevantMemory(ctx, sessionID, goal)
		if err == nil && memCtx != "" {
			msgs = append(msgs, types.Message{Role: "system", Content: "Relevant Memory Context:\n" + memCtx})
		}
	}

	// 4. 短期对话历史及工具结果
	msgs = append(msgs, shortTermHistory...)

	return msgs, nil
}
