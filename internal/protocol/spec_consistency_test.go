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
	expected := map[string]TaintLevel{
		"none":          TaintNone,
		"low":           TaintLow,
		"medium":        TaintMedium,
		"high":          TaintHigh,
		"user_reviewed": TaintUserReviewed,
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
	expected := map[string]AgentState{
		"s_idle":      AgentStateIdle,
		"s_perceive":  AgentStatePerceive,
		"s_plan":      AgentStatePlan,
		"s_validate":  AgentStateValidate,
		"s_execute":   AgentStateExecute,
		"s_reflect":   AgentStateReflect,
		"s_replan":    AgentStateReplan,
		"s_rollback":  AgentStateRollback,
		"s_complete":  AgentStateComplete,
		"s_failed":    AgentStateFailed,
		"s_interrupt": AgentStateInterrupt,
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
