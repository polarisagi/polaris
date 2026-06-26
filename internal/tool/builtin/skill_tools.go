package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// RegisterSkillTools 注册技能生成与保存工具。
// outbox 可为 nil（LogicCollapse 功能不可用时降级为日志记录）。
func RegisterSkillTools(
	sbx *sandbox.InProcessSandbox,
	toolReg *tool.InMemoryToolRegistry,
	skillReg protocol.SkillRegistry,
	outbox protocol.OutboxWriter,
) error {
	type entry struct {
		tool types.Tool
		fn   sandbox.InProcessFn
	}

	entries := []entry{
		{tool: skillSaveTool(), fn: makeSkillSaveFn(skillReg)},
		{tool: skillGenerateTool(), fn: makeSkillGenerateFn(outbox)},
	}

	for _, e := range entries {
		sbx.Register(e.tool.Name, e.fn)
		if err := toolReg.Register(e.tool); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "skill_tools: register "+e.tool.Name, err)
		}
	}
	return nil
}

func skillSaveTool() types.Tool {
	return types.Tool{
		Name: "skill_save",
		Description: "Save a verified script as a reusable skill. " +
			"Use this only when you have successfully executed and verified a complex workflow script " +
			"and want to persist it for future use. The script must be self-contained.",
		Version:     "1.0.0",
		Source:      types.ToolBuiltin,
		Capability:  types.CapWriteLocal,
		RiskLevel:   types.RiskMedium,
		SandboxTier: types.SandboxInProcess,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"skill_name": map[string]any{
					"type":        "string",
					"description": "A unique, descriptive name for the skill (e.g., 'git_commit_summarizer')",
				},
				"description": map[string]any{
					"type":        "string",
					"description": "What the skill does and when to use it",
				},
				"script_content": map[string]any{
					"type":        "string",
					"description": "The actual script code (Python or JavaScript)",
				},
				"language": map[string]any{
					"type":        "string",
					"description": "The programming language of the script (python, javascript, bash)",
				},
				"version": map[string]any{
					"type":        "string",
					"description": "Optional version (e.g. 1 or 2). Auto-increments if omitted.",
				},
			},
			"required": []string{"skill_name", "description", "script_content", "language"},
		},
	}
}

type skillSaveArgs struct {
	SkillName     string `json:"skill_name"`
	Description   string `json:"description"`
	ScriptContent string `json:"script_content"`
	Language      string `json:"language"`
	Version       string `json:"version,omitempty"`
}

func makeSkillSaveFn(skillReg protocol.SkillRegistry) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args skillSaveArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "skill_save: invalid args", err)
		}

		skillName := fmt.Sprintf("skill:%s", args.SkillName)
		var newVersion string
		if args.Version != "" { //nolint:nestif
			newVersion = args.Version
		} else {
			var v int64 = 1
			old, err := skillReg.Get(ctx, skillName, "")
			if err == nil && old != nil {
				if parsed, parseErr := strconv.ParseInt(old.Version, 10, 64); parseErr == nil {
					v = parsed + 1
				} else {
					v = 2 // fallback if parsing failed, assuming 1.0.0 or similar
				}
			}
			newVersion = strconv.FormatInt(v, 10)
		}

		meta := types.SkillMeta{
			Name:         skillName,
			Version:      newVersion,
			Runtime:      "script",
			RiskLevel:    "medium",
			Sandbox:      1,
			Capabilities: []string{"execute"},
			ExecMode:     "tool",
			Trust:        types.TrustLocal,
			Instructions: args.Description + "\n\n" + args.ScriptContent,
		}

		if err := skillReg.Register(ctx, meta); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "skill_save: register failed", err)
		}

		return []byte(`{"status":"success", "message":"skill saved to registry"}`), nil
	}
}

func skillGenerateTool() types.Tool {
	return types.Tool{
		Name: "skill_generate",
		Description: "Trigger the logic collapse engine to automatically generate a fast-path skill " +
			"based on recent successful executions. This is an explicit self-improvement signal.",
		Version:     "1.0.0",
		Source:      types.ToolBuiltin,
		Capability:  types.CapWriteLocal,
		RiskLevel:   types.RiskMedium,
		SandboxTier: types.SandboxInProcess,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_type": map[string]any{
					"type":        "string",
					"description": "The type of task to generate a skill for",
				},
				"reasoning": map[string]any{
					"type":        "string",
					"description": "Why this task type is a good candidate for logic collapse",
				},
			},
			"required": []string{"task_type", "reasoning"},
		},
	}
}

type skillGenerateArgs struct {
	TaskType  string `json:"task_type"`
	Reasoning string `json:"reasoning"`
}

func makeSkillGenerateFn(outbox protocol.OutboxWriter) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args skillGenerateArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "skill_generate: invalid args", err)
		}

		// 向 M9 引擎发送显式 Logic Collapse 触发信号。
		// OutboxWorker 将此事件路由到 GapFillWorker（m9_capability_gap handler）。
		// outbox 为 nil 时（LogicCollapse 功能未激活），降级为 no-op。
		if outbox != nil {
			payload, _ := json.Marshal(map[string]string{
				"task_type": args.TaskType,
				"reasoning": args.Reasoning,
				"trigger":   "agent_explicit",
			})
			if err := outbox.Write(ctx, protocol.OutboxEntry{
				TargetEngine: "m9_logic_collapse",
				Operation:    "trigger",
				Scope:        args.TaskType,
				Payload:      payload,
			}); err != nil {
				// 非致命：outbox 写入失败不阻断工具响应
				return []byte(fmt.Sprintf(`{"status":"queued_with_warning","message":"signal sent but outbox write failed: %s"}`, err.Error())), nil
			}
		}

		return []byte(fmt.Sprintf(`{"status":"success","message":"Logic collapse signal queued for task_type=%s"}`, args.TaskType)), nil
	}
}
