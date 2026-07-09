package protocol

// spec_consistency_test 守护 docs/arch/spec/state.yaml ↔ Go 代码的一致性。
// 设计依据: docs/arch/decisions/ADR-0012-spec-consistency-test.md
// SSoT 决策: docs/arch/decisions/ADR-0006-state-yaml-ssot.md
//
// 当前覆盖 Tier 1（CI fail-closed）:
//   - taint.levels        ↔ TaintLevel 枚举（含 ord 值精确匹配）
//   - par.states          ↔ AgentState 枚举
//   - par.transitions     ↔ from/to 必须引用已定义的状态（结构性校验）
//   - kill_switch.stages  ↔ KillState 三阶段（不含隐式 Normal base）
//
// Tier 2/3 在后续迭代增量补充——见 ADR-0012 §测试范围分级。

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/polarisagi/polaris/pkg/types"
)

// stateSpec 是 state.yaml 的部分映射（按 Tier 1 需要解构）。
type stateSpec struct {
	Par struct {
		States      []string                 `yaml:"states"`
		Transitions []map[string]interface{} `yaml:"transitions"`
	} `yaml:"par"`

	Taint struct {
		Levels []taintLevelEntry `yaml:"levels"`
	} `yaml:"taint"`

	KillSwitch struct {
		Stages map[string]map[string]interface{} `yaml:"stages"`
	} `yaml:"kill_switch"`

	TaskStatus struct {
		States []string `yaml:"states"`
	} `yaml:"task_status"`

	Outbox struct {
		States []string `yaml:"states"`
	} `yaml:"outbox"`
}

type taintLevelEntry struct {
	Name string `yaml:"name"`
	Ord  int    `yaml:"ord"`
}

// loadStateSpec 定位并解析 docs/arch/spec/state.yaml。
// 路径用 runtime.Caller 锚定本文件位置，跨平台稳定。
func loadStateSpec(t *testing.T) *stateSpec {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	specPath := filepath.Join(filepath.Dir(file), "..", "..", "docs", "arch", "spec", "state.yaml")
	raw, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("读取 state.yaml 失败: %v (路径=%s)", err, specPath)
	}
	var spec stateSpec
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("解析 state.yaml 失败: %v", err)
	}
	return &spec
}

// TestSpecTaintLevels 验证 state.yaml taint.levels 与 Go TaintLevel 枚举一致。
// 名称匹配（snake_case ↔ PascalCase 显式映射）+ ord 值精确等值。
func TestSpecTaintLevels(t *testing.T) {
	spec := loadStateSpec(t)
	// state.yaml 用 snake_case，Go 用 PascalCase；显式映射避免歧义
	expected := map[string]types.TaintLevel{
		"none":          types.TaintNone,
		"low":           types.TaintLow,
		"medium":        types.TaintMedium,
		"high":          types.TaintHigh,
		"user_reviewed": types.TaintUserReviewed,
	}
	if len(spec.Taint.Levels) != len(expected) {
		t.Fatalf("taint.levels 数量 %d ≠ Go TaintLevel 枚举数 %d", len(spec.Taint.Levels), len(expected))
	}
	for _, lvl := range spec.Taint.Levels {
		goLvl, ok := expected[lvl.Name]
		if !ok {
			t.Errorf("state.yaml taint.levels 含 %q，Go TaintLevel 未定义对应常量", lvl.Name)
			continue
		}
		if int(goLvl) != lvl.Ord {
			t.Errorf("taint.levels[%s].ord = %d，Go %s = %d（应相等）",
				lvl.Name, lvl.Ord, lvl.Name, int(goLvl))
		}
	}
}

// TestSpecParStates 验证 state.yaml par.states 与 Go AgentState 枚举集合一致。
// 双向校验: yaml 中每项必有 Go 对应；Go 中每项必在 yaml 中。
func TestSpecParStates(t *testing.T) {
	spec := loadStateSpec(t)
	expected := map[string]types.AgentState{
		"s_idle":      types.AgentStateIdle,
		"s_perceive":  types.AgentStatePerceive,
		"s_plan":      types.AgentStatePlan,
		"s_validate":  types.AgentStateValidate,
		"s_execute":   types.AgentStateExecute,
		"s_reflect":   types.AgentStateReflect,
		"s_replan":    types.AgentStateReplan,
		"s_rollback":  types.AgentStateRollback,
		"s_complete":  types.AgentStateComplete,
		"s_failed":    types.AgentStateFailed,
		"s_interrupt": types.AgentStateInterrupt,
		"s_suspended": types.AgentStateSuspended,
	}
	yamlSet := make(map[string]bool, len(spec.Par.States))
	for _, s := range spec.Par.States {
		yamlSet[s] = true
	}
	// Go → yaml 方向
	for name, val := range expected {
		if !yamlSet[name] {
			t.Errorf("Go AgentState %q（值=%d）未在 state.yaml par.states 中定义", name, val)
		}
	}
	// yaml → Go 方向
	for _, name := range spec.Par.States {
		if _, ok := expected[name]; !ok {
			t.Errorf("state.yaml par.states 含 %q，Go AgentState 枚举未定义", name)
		}
	}
}

// TestSpecParTransitionsReferenceKnownStates 验证每条 transition 的 from/to 引用已定义的状态。
// 结构性校验，不强制每个 Go transition 与 yaml 1:1 对应（Tier 2 范畴）。
func TestSpecParTransitionsReferenceKnownStates(t *testing.T) {
	spec := loadStateSpec(t)
	states := make(map[string]bool, len(spec.Par.States))
	for _, s := range spec.Par.States {
		states[s] = true
	}
	// s_error 是 LLM fill 失败时的内部错误终态指代（见 par.transitions effect.on_failure），
	// 不在 par.states 显式列出但允许在 transitions 中引用
	allowedExtras := map[string]bool{"s_error": true}
	for i, tr := range spec.Par.Transitions {
		if from, ok := tr["from"].(string); ok {
			if !states[from] && !allowedExtras[from] {
				t.Errorf("par.transitions[%d].from = %q 未在 par.states 中定义", i, from)
			}
		}
		if to, ok := tr["to"].(string); ok {
			if !states[to] && !allowedExtras[to] {
				t.Errorf("par.transitions[%d].to = %q 未在 par.states 中定义", i, to)
			}
		}
	}
}

// TestSpecKillSwitchStages 验证 state.yaml kill_switch.stages 三阶段定义存在。
// Go 侧 KillState 含隐式 KillNormal base（yaml 未显式列），仅校验 3 个非 Normal 阶段对齐。
func TestSpecKillSwitchStages(t *testing.T) {
	spec := loadStateSpec(t)
	expected := []string{
		"Stage1_THROTTLE",
		"Stage2_PAUSE",
		"Stage3_FULLSTOP",
	}
	if len(spec.KillSwitch.Stages) != len(expected) {
		t.Fatalf("kill_switch.stages 数量 %d ≠ 期望 %d (Stage 1/2/3，Normal 隐式)",
			len(spec.KillSwitch.Stages), len(expected))
	}
	for _, name := range expected {
		if _, ok := spec.KillSwitch.Stages[name]; !ok {
			t.Errorf("kill_switch.stages 缺 %q", name)
		}
	}
}

// TestSpecTaskStatus 验证 state.yaml task_status 与 Go TaskStatus 枚举集合一致。
func TestSpecTaskStatus(t *testing.T) {
	spec := loadStateSpec(t)
	expected := map[string]types.TaskStatus{
		"pending":      types.TaskPending,
		"claimed":      types.TaskClaimed,
		"executing":    types.TaskExecuting,
		"done":         types.TaskDone,
		"failed":       types.TaskFailed,
		"suspended":    types.TaskSuspended,
		"compensating": types.TaskCompensating,
	}
	yamlSet := make(map[string]bool, len(spec.TaskStatus.States))
	for _, s := range spec.TaskStatus.States {
		yamlSet[s] = true
	}
	// Go → yaml
	for name, val := range expected {
		if !yamlSet[name] {
			t.Errorf("Go TaskStatus %q（值=%d）未在 state.yaml task_status.states 中定义", name, val)
		}
	}
	// yaml → Go
	for _, name := range spec.TaskStatus.States {
		if _, ok := expected[name]; !ok {
			t.Errorf("state.yaml task_status.states 含 %q，Go TaskStatus 枚举未定义", name)
		}
	}
}

// TestSpecOutboxStatus 验证 state.yaml outbox 与 Go OutboxStatus 枚举集合一致。
func TestSpecOutboxStatus(t *testing.T) {
	spec := loadStateSpec(t)
	expected := map[string]types.OutboxStatus{
		"pending":    types.OutboxPending,
		"processing": types.OutboxProcessing,
		"done":       types.OutboxDone,
		"dead":       types.OutboxDead,
	}
	yamlSet := make(map[string]bool, len(spec.Outbox.States))
	for _, s := range spec.Outbox.States {
		yamlSet[s] = true
	}
	// Go → yaml
	for name, val := range expected {
		if !yamlSet[name] {
			t.Errorf("Go OutboxStatus %q（值=%q）未在 state.yaml outbox.states 中定义", name, val)
		}
	}
	// yaml → Go
	for _, name := range spec.Outbox.States {
		if _, ok := expected[name]; !ok {
			t.Errorf("state.yaml outbox.states 含 %q，Go OutboxStatus 枚举未定义", name)
		}
	}
}

// TestSpecParTransitionsGoImplementation 验证 state.yaml par.transitions 中的事件枚举能映射到 Go 中
func TestSpecParTransitionsGoImplementation(t *testing.T) {
	spec := loadStateSpec(t)

	expectedStates := map[string]types.AgentState{
		"s_idle":      types.AgentStateIdle,
		"s_perceive":  types.AgentStatePerceive,
		"s_plan":      types.AgentStatePlan,
		"s_validate":  types.AgentStateValidate,
		"s_execute":   types.AgentStateExecute,
		"s_reflect":   types.AgentStateReflect,
		"s_replan":    types.AgentStateReplan,
		"s_rollback":  types.AgentStateRollback,
		"s_complete":  types.AgentStateComplete,
		"s_failed":    types.AgentStateFailed,
		"s_interrupt": types.AgentStateInterrupt,
		"s_suspended": types.AgentStateSuspended,
	}

	expectedTriggers := map[string]types.AgentTrigger{
		"new_intent":                types.TriggerIntentReceived,
		"perceive_done":             types.TriggerPerceiveDone,
		"plan_done":                 types.TriggerPlanDone,
		"validate_pass":             types.TriggerValidateOk,
		"validate_fail":             types.TriggerValidateFail,
		"all_steps_success":         types.TriggerExecuteDone,
		"step_failed_unrecoverable": types.TriggerExecuteFail,
		"reflect_done":              types.TriggerReflectDone,
		"rollback_done":             types.TriggerRollbackDone,
		"replan_done":               types.TriggerReplanDone,
		"replan_exhausted":          types.TriggerReplanExhausted,
		"interrupt_received":        types.TriggerInterruptReceived,
		"interrupt_resume":          types.TriggerInterruptResume,
		"interrupt_abort":           types.TriggerInterruptAbort,
		"suspend":                   types.TriggerSuspend,
		"resume":                    types.TriggerResume,
	}

	allowedExtraEvents := map[string]bool{
		"terminal":                true, // Implicit state machine cleanup
		"step_failed_recoverable": true, // Internal retry loop without explicit state change trigger
	}

	allowedExtraStates := map[string]bool{
		"s_error": true, // Internal state transition for llm fill failure
	}

	for i, tr := range spec.Par.Transitions {
		fromStr, _ := tr["from"].(string)
		toStr, _ := tr["to"].(string)
		eventStr, _ := tr["event"].(string)

		if _, ok := expectedStates[fromStr]; !ok && !allowedExtraStates[fromStr] {
			t.Errorf("par.transitions[%d].from = %q 未在 Go AgentState 枚举中定义", i, fromStr)
		}

		if _, ok := expectedStates[toStr]; !ok && !allowedExtraStates[toStr] {
			t.Errorf("par.transitions[%d].to = %q 未在 Go AgentState 枚举中定义", i, toStr)
		}

		if _, ok := expectedTriggers[eventStr]; !ok && !allowedExtraEvents[eventStr] {
			t.Errorf("par.transitions[%d].event = %q 未在 Go AgentTrigger 枚举中定义", i, eventStr)
		}
	}
}
