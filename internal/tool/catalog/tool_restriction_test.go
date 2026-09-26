package catalog

import (
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestToolRestriction_ClaudeNames(t *testing.T) {
	r := NewToolRestriction([]string{"Read", "Grep", "Bash(git *)", "mcp__github", "Skill(pdf)", "Agent(worker)"}, []string{"Write"})
	cases := []struct {
		name string
		src  types.ToolSource
		want bool
	}{
		{"read_file", types.ToolBuiltin, true},
		{"grep", types.ToolBuiltin, true},
		{"bash", types.ToolBuiltin, false}, // 参数级规则不放宽为整个工具
		{"code_act:python", types.ToolBuiltin, false},
		{"mcp__github__create_issue", types.ToolMCP, true},
		{"mcp__gitlab__x", types.ToolMCP, false},
		{"skill__pdf", types.ToolSkill, true},
		{"skill__other", types.ToolSkill, false},
		{"write_file", types.ToolBuiltin, false},
		{DelegateToolName, types.ToolBuiltin, true},
	}
	for _, c := range cases {
		if got := r.Permits(c.name, c.src); got != c.want {
			t.Errorf("Permits(%s)=%v want %v", c.name, got, c.want)
		}
	}
	if len(r.Ignored) != 1 || r.Ignored[0] != "Bash(git *)" {
		t.Fatalf("ignored: %v", r.Ignored)
	}
	if !r.PermitsAgent("worker") || r.PermitsAgent("other") {
		t.Fatal("Agent(worker) must restrict delegation targets")
	}
}

func TestToolRestriction_InheritAndDeny(t *testing.T) {
	if NewToolRestriction(nil, nil) != nil {
		t.Fatal("no lists = no restriction")
	}
	r := NewToolRestriction(nil, []string{"Bash(rm *)", "Skill"})
	if r.Permits("bash", types.ToolBuiltin) || r.Permits("code_act:js", "") || r.Permits("skill__x", types.ToolSkill) {
		t.Fatal("deny entries must block the whole tool")
	}
	if !r.Permits("read_file", types.ToolBuiltin) || !r.PermitsAgent("any") {
		t.Fatal("tools omitted = inherit everything else")
	}
}
