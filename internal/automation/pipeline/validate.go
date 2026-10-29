package pipeline

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// ToolMeta 描述工具在管线内的元数据判定（消费方适配器提供）。
type ToolMeta struct {
	Name         string
	ReadOnly     bool
	WriteAllowed bool
	Privileged   bool
	Exists       bool
}

// ValidateDeps 提供校验所需的外部只读上下文。
type ValidateDeps interface {
	ToolMeta(name string) ToolMeta
}

// Limits 校验限制阈值。
type Limits struct {
	MaxSteps           int
	MinIntervalMin     int
	LLMMaxInputChars   int
	LLMMaxOutputTokens int
	StepDataMaxBytes   int
}

// FieldError 静态校验失败的单条错误描述。
type FieldError struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

var (
	stepKeyRegex     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	templateVarRegex = regexp.MustCompile(`\{\{\s*([a-z_.0-9]+)\s*\}\}`)
)

func isReservedStepKey(k string) bool {
	return k == "run" || k == "item" || k == "steps"
}

// Validate 静态校验 Spec。返回所有发现的校验错误，若全部合规返回 nil 或空切片。
//
//nolint:gocyclo,nestif
func Validate(s Spec, deps ValidateDeps, lim Limits, minInterval func(cron string) (int, error)) []FieldError {
	var errs []FieldError

	addErr := func(path, code, msg string) {
		errs = append(errs, FieldError{
			Path:    path,
			Code:    code,
			Message: msg,
		})
	}

	// 1. 基本信息校验
	if strings.TrimSpace(s.Name) == "" {
		addErr("name", "required", "name is required")
	} else if len(s.Name) > 60 {
		addErr("name", "out_of_range", "name must be <= 60 chars")
	}

	// 2. 触发器校验
	if s.Trigger.Type == "" {
		addErr("trigger.type", "required", "trigger type is required")
	} else if s.Trigger.Type != "cron" && s.Trigger.Type != "manual" {
		addErr("trigger.type", "invalid_enum", "trigger type must be cron or manual")
	}
	if s.Trigger.Type == "cron" {
		if strings.TrimSpace(s.Trigger.Cron) == "" {
			addErr("trigger.cron", "required", "cron schedule is required when trigger type is cron")
		} else if minInterval != nil {
			interval, err := minInterval(s.Trigger.Cron)
			if err != nil {
				addErr("trigger.cron", "invalid_format", fmt.Sprintf("invalid cron schedule: %v", err))
			} else if lim.MinIntervalMin > 0 && interval < lim.MinIntervalMin {
				addErr("trigger.cron", "out_of_range", fmt.Sprintf("cron interval %d min is below minimum %d min", interval, lim.MinIntervalMin))
			}
		}
	}

	// 3. 投递动作校验
	if s.ResultAction != "" && s.ResultAction != "session" && s.ResultAction != "silent" {
		if !strings.HasPrefix(s.ResultAction, "channel:") || len(s.ResultAction) <= 8 {
			addErr("result_action", "invalid_enum", "result_action must be 'session', 'silent', or 'channel:<id>'")
		}
	}

	// 4. 步骤数量与 Key 唯一性校验
	if len(s.Steps) == 0 {
		addErr("steps", "required", "steps cannot be empty")
		return errs
	}
	if lim.MaxSteps > 0 && len(s.Steps) > lim.MaxSteps {
		addErr("steps", "out_of_range", fmt.Sprintf("steps count %d exceeds maximum %d", len(s.Steps), lim.MaxSteps))
	}

	stepKeyIndices := make(map[string]int)
	for i, st := range s.Steps {
		stepPath := fmt.Sprintf("steps[%d].key", i)
		if st.Key == "" {
			addErr(stepPath, "required", "step key is required")
			continue
		}
		if !stepKeyRegex.MatchString(st.Key) {
			addErr(stepPath, "invalid_format", fmt.Sprintf("step key %q must match ^[a-z][a-z0-9_]{0,31}$", st.Key))
		}
		if isReservedStepKey(st.Key) {
			addErr(stepPath, "not_allowed", fmt.Sprintf("step key %q is reserved", st.Key))
		}
		if _, exists := stepKeyIndices[st.Key]; exists {
			addErr(stepPath, "invalid_ref", fmt.Sprintf("duplicate step key %q", st.Key))
		} else {
			stepKeyIndices[st.Key] = i
		}
	}

	// 5. 依赖拓扑校验（必须引用在自身之前声明的 key，防止后向依赖与循环依赖）
	for i, st := range s.Steps {
		for ni, need := range st.Needs {
			needPath := fmt.Sprintf("steps[%d].needs[%d]", i, ni)
			prevIdx, exists := stepKeyIndices[need]
			if !exists {
				addErr(needPath, "invalid_ref", fmt.Sprintf("unknown step key %q in needs", need))
			} else if prevIdx >= i {
				addErr(needPath, "invalid_ref", fmt.Sprintf("need %q must be declared before step %q", need, st.Key))
			}
		}
	}

	// 6. 投递目标步骤校验
	if s.DeliverFrom != "" {
		targetIdx, exists := stepKeyIndices[s.DeliverFrom]
		if !exists {
			addErr("deliver_from", "invalid_ref", fmt.Sprintf("deliver_from step %q does not exist", s.DeliverFrom))
		} else {
			targetKind := s.Steps[targetIdx].Kind
			if targetKind != "llm" && targetKind != "agent" {
				addErr("deliver_from", "invalid_ref", fmt.Sprintf("deliver_from step %q must be of kind llm or agent, got %q", s.DeliverFrom, targetKind))
			}
		}
	}

	// 7. 单步规格校验
	for i, st := range s.Steps {
		stepPath := fmt.Sprintf("steps[%d]", i)
		switch st.Kind {
		case "tool":
			if st.Tool == "" {
				addErr(stepPath+".tool", "required", "tool name is required")
			} else {
				if strings.HasPrefix(st.Tool, "automation_") || st.Tool == "cron_create" || st.Tool == "cron_delete" || st.Tool == "cron_list" {
					addErr(stepPath+".tool", "not_allowed", fmt.Sprintf("tool %q is not allowed in pipeline", st.Tool))
				} else if deps != nil {
					meta := deps.ToolMeta(st.Tool)
					if !meta.Exists {
						addErr(stepPath+".tool", "invalid_ref", fmt.Sprintf("tool %q does not exist", st.Tool))
					} else if meta.Privileged {
						addErr(stepPath+".tool", "not_allowed", fmt.Sprintf("privileged tool %q is not allowed in pipeline", st.Tool))
					} else if !meta.ReadOnly && !meta.WriteAllowed && st.Approval != "hitl" {
						addErr(stepPath+".tool", "not_allowed", fmt.Sprintf("write tool %q requires approval=hitl", st.Tool))
					}
				}
			}
			if len(st.Args) > 0 {
				var dummy any
				if err := json.Unmarshal(st.Args, &dummy); err != nil {
					addErr(stepPath+".args", "invalid_format", "args must be valid JSON")
				}
				validateTemplateRefs(st.Args, stepPath+".args", st.Needs, addErr)
			}

		case "transform":
			if len(st.Ops) == 0 {
				addErr(stepPath+".ops", "required", "transform step requires at least one op")
			}
			for oi, op := range st.Ops {
				opPath := fmt.Sprintf("%s.ops[%d]", stepPath, oi)
				switch op.Op {
				case "concat":
					if len(op.From) == 0 {
						addErr(opPath+".from", "required", "concat requires from")
					}
					for fi, f := range op.From {
						if !sliceContains(st.Needs, f) {
							addErr(fmt.Sprintf("%s.from[%d]", opPath, fi), "invalid_ref", fmt.Sprintf("from %q must be in step needs", f))
						}
					}
				case "dedup":
					if op.By == "" {
						addErr(opPath+".by", "required", "dedup requires by")
					}
					if op.Scope != "" && op.Scope != "run" && op.Scope != "workflow" {
						addErr(opPath+".scope", "invalid_enum", "scope must be run or workflow")
					}
				case "filter":
					if op.Field == "" {
						addErr(opPath+".field", "required", "filter requires field")
					}
					validCmps := map[string]bool{
						"==": true, "!=": true, ">": true, ">=": true, "<": true, "<=": true,
						"contains": true, "not_contains": true, "regex": true,
					}
					if !validCmps[op.Cmp] {
						addErr(opPath+".cmp", "invalid_enum", fmt.Sprintf("invalid cmp operator %q", op.Cmp))
					}
					if op.Cmp == "regex" {
						sVal, ok := op.Value.(string)
						if !ok {
							addErr(opPath+".value", "invalid_format", "regex value must be string")
						} else if _, err := regexp.Compile(sVal); err != nil {
							addErr(opPath+".value", "invalid_format", fmt.Sprintf("invalid regex pattern: %v", err))
						}
					}
				case "sort":
					if op.By == "" {
						addErr(opPath+".by", "required", "sort requires by")
					}
				case "limit":
					if op.N < 1 {
						addErr(opPath+".n", "out_of_range", "limit n must be >= 1")
					}
				case "pick":
					if len(op.Fields) == 0 {
						addErr(opPath+".fields", "required", "pick requires fields")
					}
				case "rename":
					if len(op.Map) == 0 {
						addErr(opPath+".map", "required", "rename requires map")
					}
				case "truncate":
					if op.Field == "" {
						addErr(opPath+".field", "required", "truncate requires field")
					}
					if op.MaxChars < 1 {
						addErr(opPath+".max_chars", "out_of_range", "truncate max_chars must be >= 1")
					}
				default:
					addErr(opPath+".op", "invalid_enum", fmt.Sprintf("unknown op %q", op.Op))
				}
			}

		case "llm":
			if st.Input == "" {
				addErr(stepPath+".input", "required", "llm step requires input")
			} else if !sliceContains(st.Needs, st.Input) {
				addErr(stepPath+".input", "invalid_ref", fmt.Sprintf("input %q must be in step needs", st.Input))
			}
			if strings.TrimSpace(st.Prompt) == "" {
				addErr(stepPath+".prompt", "required", "llm step requires prompt")
			} else if len(st.Prompt) > 4000 {
				addErr(stepPath+".prompt", "out_of_range", "prompt exceeds 4000 characters")
			}
			if st.Output != "" && st.Output != "markdown" && st.Output != "json" {
				addErr(stepPath+".output", "invalid_enum", "output must be markdown or json")
			}
			if st.Thinking != "" && st.Thinking != "off" && st.Thinking != "low" {
				addErr(stepPath+".thinking", "invalid_enum", "thinking must be off or low")
			}
			if lim.LLMMaxInputChars > 0 && st.MaxInputChars > lim.LLMMaxInputChars {
				addErr(stepPath+".max_input_chars", "out_of_range", fmt.Sprintf("max_input_chars %d exceeds limit %d", st.MaxInputChars, lim.LLMMaxInputChars))
			}
			if lim.LLMMaxOutputTokens > 0 && st.MaxOutputTokens > lim.LLMMaxOutputTokens {
				addErr(stepPath+".max_output_tokens", "out_of_range", fmt.Sprintf("max_output_tokens %d exceeds limit %d", st.MaxOutputTokens, lim.LLMMaxOutputTokens))
			}

		case "agent":
			if strings.TrimSpace(st.Prompt) == "" {
				addErr(stepPath+".prompt", "required", "agent step requires prompt")
			} else if len(st.Prompt) > 4000 {
				addErr(stepPath+".prompt", "out_of_range", "prompt exceeds 4000 characters")
			}
			if st.ReasoningEffort != "" && st.ReasoningEffort != "low" && st.ReasoningEffort != "medium" && st.ReasoningEffort != "high" && st.ReasoningEffort != "ultra" {
				addErr(stepPath+".reasoning_effort", "invalid_enum", "invalid reasoning_effort")
			}

		default:
			addErr(stepPath+".kind", "invalid_enum", fmt.Sprintf("unknown step kind %q", st.Kind))
		}
	}

	return errs
}

func sliceContains(s []string, item string) bool {
	for _, v := range s {
		if v == item {
			return true
		}
	}
	return false
}

// validateTemplateRefs 递归检查参数 JSON 中的 {{steps.<k>.*}} 引用，确保 k 在 needs 集合内。
func validateTemplateRefs(raw json.RawMessage, path string, needs []string, addErr func(path, code, msg string)) {
	var val any
	if err := json.Unmarshal(raw, &val); err != nil {
		return
	}
	checkValueTemplateRefs(val, path, needs, addErr)
}

//nolint:nestif
func checkValueTemplateRefs(v any, path string, needs []string, addErr func(path, code, msg string)) {
	switch val := v.(type) {
	case string:
		matches := templateVarRegex.FindAllStringSubmatch(val, -1)
		for _, m := range matches {
			if len(m) > 1 {
				ref := m[1]
				if strings.HasPrefix(ref, "steps.") {
					parts := strings.Split(ref, ".")
					if len(parts) >= 2 {
						k := parts[1]
						if !sliceContains(needs, k) {
							addErr(path, "invalid_ref", fmt.Sprintf("referenced step %q must be in step needs", k))
						}
					}
				}
			}
		}
	case []any:
		for i, item := range val {
			checkValueTemplateRefs(item, fmt.Sprintf("%s[%d]", path, i), needs, addErr)
		}
	case map[string]any:
		for k, item := range val {
			checkValueTemplateRefs(item, fmt.Sprintf("%s.%s", path, k), needs, addErr)
		}
	}
}
