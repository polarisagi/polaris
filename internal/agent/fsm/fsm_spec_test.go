package fsm

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/tool/catalog"
	"github.com/polarisagi/polaris/pkg/types"
)

func stateYamlPath(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")
	p := filepath.Join(root, "docs", "arch", "spec", "state.yaml")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("state.yaml not found: %v", err)
	}
	return p
}

type yamlState struct {
	Par struct {
		Initial     string   `yaml:"initial"`
		States      []string `yaml:"states"`
		Transitions []struct {
			From  string `yaml:"from"`
			Event string `yaml:"event"`
			To    string `yaml:"to"`
		} `yaml:"transitions"`
	} `yaml:"par"`
}

func stateToString(s types.AgentState) string {
	switch s {
	case types.AgentStateIdle:
		return "s_idle"
	case types.AgentStatePerceive:
		return "s_perceive"
	case types.AgentStatePlan:
		return "s_plan"
	case types.AgentStateValidate:
		return "s_validate"
	case types.AgentStateExecute:
		return "s_execute"
	case types.AgentStateReflect:
		return "s_reflect"
	case types.AgentStateReplan:
		return "s_replan"
	case types.AgentStateRollback:
		return "s_rollback"
	case types.AgentStateComplete:
		return "s_complete"
	case types.AgentStateFailed:
		return "s_failed"
	case types.AgentStateInterrupt:
		return "s_interrupt"
	case types.AgentStateSuspended:
		return "s_suspended"
	default:
		return "unknown"
	}
}

func triggerToString(t types.AgentTrigger) string {
	switch t {
	case types.TriggerIntentReceived:
		return "new_intent"
	case types.TriggerPerceiveDone:
		return "perceive_done"
	case types.TriggerPlanDone:
		return "plan_done"
	case types.TriggerValidateOk:
		return "validate_pass"
	case types.TriggerValidateFail:
		return "validate_fail"
	case types.TriggerExecuteDone:
		return "all_steps_success"
	case types.TriggerExecuteFail:
		return "step_failed_unrecoverable"
	case types.TriggerReflectDone:
		return "reflect_done"
	case types.TriggerRollbackDone:
		return "rollback_done"
	case types.TriggerReplanDone:
		return "replan_done"
	case types.TriggerReplanExhausted:
		return "replan_exhausted"
	case types.TriggerInterruptReceived:
		return "interrupt_received"
	case types.TriggerInterruptResume:
		return "interrupt_resume"
	case types.TriggerInterruptAbort:
		return "interrupt_abort"
	case types.TriggerSuspend:
		return "suspend"
	case types.TriggerResume:
		return "resume"
	default:
		return "unknown"
	}
}

func TestFSM_Spec(t *testing.T) {
	p := stateYamlPath(t)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read state.yaml: %v", err)
	}
	var ys yamlState
	if err := yaml.Unmarshal(b, &ys); err != nil {
		t.Fatalf("unmarshal state.yaml: %v", err)
	}

	sm := NewStateMachine(&dummyContextBuilder{})

	actualStates := make(map[string]bool)
	type trKey struct{ from, trigger, to string }
	actualTransitions := make(map[trKey]bool)

	sm.mu.Lock()
	for from, m := range sm.transitions {
		fromStr := stateToString(from)
		actualStates[fromStr] = true
		for trigger, transition := range m {
			triggerStr := triggerToString(trigger)
			toStr := stateToString(transition.To)
			actualTransitions[trKey{fromStr, triggerStr, toStr}] = true
			actualStates[toStr] = true
		}
	}
	sm.mu.Unlock()

	exemptStates := map[string]bool{
		"s_interrupt": true, // Implicitly handled in Dispatch
	}

	exemptTransitions := map[trKey]bool{
		{"s_validate", "validate_fail", "s_failed"}:            true,
		{"s_execute", "step_failed_recoverable", "s_execute"}:  true,
		{"s_execute", "step_failed_unrecoverable", "s_failed"}: true,
		{"s_complete", "terminal", "s_idle"}:                   true,
		{"s_rollback", "rollback_done", "s_failed"}:            true,
		{"s_failed", "terminal", "s_idle"}:                     true,
	}

	expectedStates := make(map[string]bool)
	for _, s := range ys.Par.States {
		expectedStates[s] = true
		if !actualStates[s] && !exemptStates[s] {
			t.Errorf("fsm_spec: yaml 状态 %s 在 StateMachine 中未注册", s)
		}
	}

	for s := range actualStates {
		if !expectedStates[s] {
			t.Logf("fsm_spec: StateMachine 中存在未在 yaml 定义的状态 %s", s)
		}
	}

	for _, tr := range ys.Par.Transitions {
		key := trKey{tr.From, tr.Event, tr.To}
		if !actualTransitions[key] && !exemptTransitions[key] {
			t.Errorf("fsm_spec: yaml 定义转移 %s --[%s]--> %s 在 StateMachine 中未找到", tr.From, tr.Event, tr.To)
		}
	}
}

type dummyContextBuilder struct{}

func (d *dummyContextBuilder) BuildPerceiveContext(ctx context.Context, memory protocol.MemoryFacade, sCtx *StateContext, cognitive CognitiveSearcher) ([]types.Message, error) {
	return nil, nil
}
func (d *dummyContextBuilder) BuildPlanContext(ctx context.Context, memory protocol.MemoryFacade, sCtx *StateContext, cata catalog.Catalog, cognitive CognitiveSearcher) ([]types.Message, error) {
	return nil, nil
}
func (d *dummyContextBuilder) BuildReflectContext(ctx context.Context, memory protocol.MemoryFacade, sCtx *StateContext) ([]types.Message, error) {
	return nil, nil
}
func (d *dummyContextBuilder) BuildToolListSection(cata catalog.Catalog) string { return "" }
