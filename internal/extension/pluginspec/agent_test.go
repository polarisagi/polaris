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
		ex.Model != "gpt-5.4" || len(ex.NotApplied) != 0 {
		t.Fatalf("codex agent: %+v", ex)
	}
	if rv.Format != AgentFormatClaude || !rv.ReadOnly || rv.MaxTurns != 5 || len(rv.Tools) != 2 ||
		rv.Model != "general" || len(rv.NotApplied) != 1 || rv.NotApplied[0] != "memory" {
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

func TestAgentModelAndEffortMapping(t *testing.T) {
	dir := t.TempDir()
	pOpus := writeAgentFile(t, dir, "opus.md", "---\nname: op\ndescription: d\nmodel: opus\neffort: high\n---\nbody")
	aOpus, _, ok := ParseAgentFile(pOpus, "op", false)
	if !ok || aOpus.Model != "reasoning" || aOpus.Thinking != "high" {
		t.Fatalf("opus mapping failed: %+v", aOpus)
	}

	pHaiku := writeAgentFile(t, dir, "haiku.md", "---\nname: hk\ndescription: d\nmodel: HAIKU\neffort: low\n---\nbody")
	aHaiku, _, ok := ParseAgentFile(pHaiku, "hk", false)
	if !ok || aHaiku.Model != "default" || aHaiku.Thinking != "low" {
		t.Fatalf("haiku mapping failed: %+v", aHaiku)
	}

	pCodex := writeAgentFile(t, dir, "codex.toml", "name = \"cdx\"\ndescription = \"d\"\ndeveloper_instructions = \"b\"\nmodel = \"sonnet\"\nmodel_reasoning_effort = \"medium\"\n")
	agents, _ := ListAgentDir(dir)
	var found bool
	for _, a := range agents {
		if a.Name == "cdx" {
			found = true
			if a.Model != "general" || a.Thinking != "medium" {
				t.Fatalf("codex mapping failed: %+v", a)
			}
		}
	}
	if !found {
		t.Fatalf("codex agent not parsed from %s", pCodex)
	}
}
