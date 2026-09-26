package pluginspec

import (
	"os"
	"path/filepath"
	"testing"
)

func writeAgentFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestListAgentDir_ClaudeAndCodex(t *testing.T) {
	dir := t.TempDir()
	writeAgentFile(t, dir, "reviewer.md", "---\nname: reviewer\ndescription: Reviews code\ntools: Read, Grep\n"+
		"permissionMode: plan\nmaxTurns: 5\nmodel: sonnet\nmemory: project\n---\nYou review code.\n")
	writeAgentFile(t, dir, "explorer.toml", "name = \"explorer\"\ndescription = \"Explores\"\n"+
		"developer_instructions = \"Explore only.\"\nsandbox_mode = \"read-only\"\nmodel = \"gpt-5.4\"\n")
	writeAgentFile(t, dir, "dup.toml", "name = \"reviewer\"\ndescription = \"x\"\ndeveloper_instructions = \"y\"\n")
	writeAgentFile(t, dir, "bad.toml", "name = \"bad\"\n")

	agents, diags := ListAgentDir(dir)
	if len(agents) != 2 {
		t.Fatalf("agents: %+v diags: %v", agents, diags)
	}
	ex, rv := agents[0], agents[1]
	if ex.Name != "explorer" || ex.Format != AgentFormatCodex || !ex.ReadOnly || ex.Body != "Explore only." ||
		len(ex.NotApplied) != 1 || ex.NotApplied[0] != "model" {
		t.Fatalf("codex agent: %+v", ex)
	}
	if rv.Format != AgentFormatClaude || !rv.ReadOnly || rv.MaxTurns != 5 || len(rv.Tools) != 2 ||
		len(rv.NotApplied) != 2 || rv.NotApplied[0] != "memory" || rv.NotApplied[1] != "model" {
		t.Fatalf("claude agent: %+v", rv)
	}
	var dupWarned, badErr bool
	for _, d := range diags {
		dupWarned = dupWarned || (d.Rule == RuleAgentParse && d.Severity == SeverityWarning)
		badErr = badErr || (d.Rule == RuleCodexAgentParse && d.Severity == SeverityError)
	}
	if !dupWarned || !badErr {
		t.Fatalf("diagnostics: %v", diags)
	}
}

func TestParseAgentFile_PluginRestrictions(t *testing.T) {
	p := writeAgentFile(t, t.TempDir(), "a.md", "---\ndescription: d\npermissionMode: plan\nhooks: {}\nmodel: inherit\n---\nbody")
	a, diags, ok := ParseAgentFile(p, "sec:a", true)
	if !ok || a.Name != "sec:a" || a.ReadOnly || a.Body != "body" {
		t.Fatalf("plugin agent: %+v %v", a, diags)
	}
	if len(a.NotApplied) != 2 || a.NotApplied[0] != "hooks" || a.NotApplied[1] != "permissionMode" {
		t.Fatalf("plugin agents ignore hooks/permissionMode; model inherit is applied: %v", a.NotApplied)
	}
}
