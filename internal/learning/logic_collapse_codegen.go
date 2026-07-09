package learning

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"

	"github.com/polarisagi/polaris/internal/protocol"
)

// defaultLLMCodeGenerator — LLM 代码生成实现（R7 拆分自
// logic_collapse_trigger.go；TrajectoryStats/LogicCollapseMonitor 触发器
// 主体见 logic_collapse_trigger.go）。

// defaultLLMCodeGenerator 使用 protocol.Provider 生成 Python 技能脚本（ADR-0026）。
type defaultLLMCodeGenerator struct {
	provider protocol.Provider
}

// NewDefaultLLMCodeGenerator 创建默认 LLM 代码生成器。
func NewDefaultLLMCodeGenerator(provider protocol.Provider) protocol.LLMCodeGenerator {
	return &defaultLLMCodeGenerator{provider: provider}
}

// GenerateImpl 将脱敏轨迹发送给 LLM 生成 Python 技能脚本（src/skill.py，ContainerSandbox 执行）。
func (g *defaultLLMCodeGenerator) GenerateImpl(ctx context.Context, traj *protocol.CollapseTrajectory) ([]byte, error) {
	if g.provider == nil {
		return nil, apperr.New(apperr.CodeInternal, "logic_collapse: LLM provider is nil")
	}

	toolCallsDesc := buildToolCallsDescription(traj.ToolCalls)
	inputSchemaDesc := buildSchemaDescription(traj.InputSchema)
	outputSchemaDesc := buildSchemaDescription(traj.OutputSchema)

	systemPrompt := `You are an AI generating a Python skill for the Polaris agent system.

STRICT REQUIREMENTS:
1. Output ONLY valid Python source code, no markdown, no explanation.
2. Define a function: def execute(input: dict) -> dict:
3. NO dynamic execution: no eval(), no exec()
4. NO direct filesystem writes or network calls unless explicitly declared in capabilities
5. Input/output must be valid JSON-serializable objects
6. The script runs via ContainerSandbox using Python 3.`

	userPrompt := fmt.Sprintf(`Generate src/skill.py for skill "%s":

Goal: %s

Tool call sequence (type signatures only):
%s

Input schema: %s
Output schema: %s

The script must implement the deterministic equivalent of this tool call sequence.`,
		traj.SkillID,
		traj.GoalDescription,
		toolCallsDesc,
		inputSchemaDesc,
		outputSchemaDesc,
	)

	req := &types.InferRequest{
		Messages: []types.Message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
	}

	//nolint:bare-infer // 历史代码暂留，后续重构替换
	resp, err := g.provider.Infer(ctx, req.Messages, types.WithMaxTokens(req.MaxTokens))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "LLM inference failed", err)
	}

	src := strings.TrimSpace(resp.Content)
	// 剥离 LLM 可能包裹的 Markdown 代码块
	src = strings.TrimPrefix(src, "```python")
	src = strings.TrimPrefix(src, "```py")
	src = strings.TrimPrefix(src, "```")
	src = strings.TrimSuffix(src, "```")
	src = strings.TrimSpace(src)

	return []byte(src), nil
}

// buildToolCallsDescription 将工具调用类型签名格式化为 LLM 可读的描述。
func buildToolCallsDescription(calls []protocol.CollapseToolCall) string {
	if len(calls) == 0 {
		return "(none)"
	}
	var sb strings.Builder
	for _, c := range calls {
		argsJSON, _ := json.Marshal(c.Args)
		fmt.Fprintf(&sb, "  %d. %s(args: %s) -> %s\n",
			c.OrderIndex+1, c.ToolName, argsJSON, c.OutputType)
	}
	return sb.String()
}

// buildSchemaDescription 将 map[string]string schema 格式化为描述。
func buildSchemaDescription(schema map[string]string) string {
	if len(schema) == 0 {
		return "{}"
	}
	b, _ := json.Marshal(schema)
	return string(b)
}
