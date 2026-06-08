package protocol

// TaskModel represents the structured output of the perception phase.
// LLM fills this slot during the S_PERCEIVE→S_PLAN transition.
type TaskModel struct {
	Goal        string   `json:"goal"`
	Context     string   `json:"context"`
	Constraints []string `json:"constraints,omitempty"`
	Priority    int      `json:"priority"`
}

// DAGModel represents the compiled execution plan.
// LLM fills this slot during the S_PLAN→S_VALIDATE transition.
type DAGModel struct {
	Nodes []DAGNode `json:"nodes"`
	Edges []DAGEdge `json:"edges"`
}

type DAGNode struct {
	ID      string         `json:"id"`
	Action  string         `json:"action"`
	Params  map[string]any `json:"params"`
	Retry   int            `json:"retry"`
	Timeout string         `json:"timeout"`
}

type DAGEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// ReflectionModel is the output of the reflection phase (S_REFLECT→S_COMPLETE).
type ReflectionModel struct {
	Success   bool     `json:"success"`
	Summary   string   `json:"summary"`
	Lessons   []string `json:"lessons,omitempty"`
	SkillName string   `json:"skill_name,omitempty"`
}

// FailureClass distinguishes uncontrollable infrastructure failures from logic errors.
// Used by self-improve and skill lifecycle to avoid punishing quality metrics on transient outages.
type FailureClass string

const (
	FailureLogic          FailureClass = "logic"          // incorrect reasoning, bad plan, skill error
	FailureControllable   FailureClass = "controllable"   // timeout, resource exhausted (system still healthy)
	FailureUncontrollable FailureClass = "uncontrollable" // network offline, provider down, quota exhausted
)
