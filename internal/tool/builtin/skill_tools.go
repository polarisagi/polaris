package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

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
		{tool: skillSaveTool(), fn: MakeSkillSaveFn(skillReg)},
		{tool: skillGenerateTool(), fn: MakeSkillGenerateFn(outbox)},
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
		TrustTier:   types.TrustSystem,
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

func MakeSkillSaveFn(skillReg protocol.SkillRegistry) sandbox.InProcessFn {
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
		TrustTier:   types.TrustSystem,
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

func MakeSkillGenerateFn(outbox protocol.OutboxWriter) sandbox.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var args skillGenerateArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "skill_generate: invalid args", err)
		}

		// 向 M9 引擎发送显式技能合成触发信号。
		// 路由到 TopicCapabilityGap——GapFillWorker 是当前唯一注册的技能合成消费者
		// （TopicLogicCollapse 无 handler 会直接落死信，见 outbox_worker.Process）。
		// error 字段格式 "tool not found: <name>" 匹配 GapFillWorker.extractMissingTool 解析规则。
		// outbox 为 nil 时（LogicCollapse 功能未激活），降级为 no-op。
		if outbox != nil {
			// 幂等键必须真正唯一（2026-07-22 一致性审查修复）：outbox.idempotency_key
			// 是 UNIQUE 约束列，空字符串会导致同一部署生命周期内只有*全局第一次*
			// 显式技能合成触发能成功写入——此后任何 task_type 的后续触发都会撞
			// UNIQUE 约束（虽然此处确有检查 err 并回传告警，不是完全静默，但真实
			// 技能合成信号从第二次起就再也没有到达过 GapFillWorker）。用
			// task_type + 纳秒时间戳：前者保留可追溯性，后者保证每次真实触发都
			// 不会与历史记录冲突。
			idemKey := fmt.Sprintf("skillgap:%s:%d", args.TaskType, time.Now().UnixNano())
			ev, _ := protocol.NewOutboxEvent(protocol.TopicCapabilityGap, "trigger", map[string]string{
				"error":     "tool not found: " + args.TaskType,
				"task_type": args.TaskType,
				"reasoning": args.Reasoning,
				"trigger":   "agent_explicit",
			}, idemKey)
			ev.Scope = args.TaskType
			if err := outbox.Write(ctx, ev); err != nil {
				// 非致命：outbox 写入失败不阻断工具响应
				return []byte(fmt.Sprintf(`{"status":"queued_with_warning","message":"signal sent but outbox write failed: %s"}`, err.Error())), nil
			}
		}

		return []byte(fmt.Sprintf(`{"status":"success","message":"Logic collapse signal queued for task_type=%s"}`, args.TaskType)), nil
	}
}
