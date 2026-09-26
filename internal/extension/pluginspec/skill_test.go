package pluginspec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasRule(ds []Diagnostic, rule string, sev Severity) bool {
	for _, d := range ds {
		if d.Rule == rule && d.Severity == sev {
			return true
		}
	}
	return false
}

func TestParseSkillDir_AgentskillsFullFields(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pdf-processing")
	writeFile(t, filepath.Join(dir, "SKILL.md"), `---
name: pdf-processing
description: Extract PDF text. Use when handling PDFs.
license: Apache-2.0
compatibility: Requires Python 3.14+
metadata:
  author: example-org
  version: "1.0"
  polaris-exec-mode: ambient
allowed-tools: Bash(git:*) Read
---
# Steps
Do things.
`)
	s, ds := ParseSkillDir(dir)
	if s == nil {
		t.Fatalf("expected skill, diags=%v", ds)
	}
	if len(ds) != 0 {
		t.Fatalf("expected no diagnostics, got %v", ds)
	}
	if s.Name != "pdf-processing" || s.License != "Apache-2.0" || s.Metadata["author"] != "example-org" {
		t.Fatalf("unexpected fields: %+v", s)
	}
	if got := strings.Join(s.AllowedTools, "|"); got != "Bash(git:*)|Read" {
		t.Fatalf("allowed-tools = %q", got)
	}
	if s.PolarisParam("exec-mode") != "ambient" {
		t.Fatalf("polaris param not read from metadata")
	}
	if !s.UserInvocable || !s.ModelInvocable() {
		t.Fatalf("defaults must be invocable by both user and model")
	}
	if !strings.HasPrefix(s.Body, "# Steps") {
		t.Fatalf("body = %q", s.Body)
	}
}

// Claude 允许省略 name / description：分别回落目录名与正文首行，只告警不阻断。
func TestParseSkillDir_ClaudeLenientDefaults(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "deploy")
	writeFile(t, filepath.Join(dir, "SKILL.md"), "---\nargument-hint: \"[env]\"\ndisable-model-invocation: yes\nuser-invocable: off\nallowed-tools: Read, Grep\npaths: \"src/**/*.go, docs/*.md\"\ncontext: fork\n---\n\n## Deploy the service\nsteps\n")
	s, ds := ParseSkillDir(dir)
	if s == nil {
		t.Fatalf("expected skill, diags=%v", ds)
	}
	if s.Name != "deploy" || s.Description != "Deploy the service" {
		t.Fatalf("defaults not applied: name=%q desc=%q", s.Name, s.Description)
	}
	if !hasRule(ds, RuleSkillDescMissing, SeverityWarning) {
		t.Fatalf("missing description warning, got %v", ds)
	}
	if s.ModelInvocable() || s.UserInvocable {
		t.Fatalf("yes/off booleans not honored")
	}
	if len(s.AllowedTools) != 2 || len(s.Paths) != 2 || s.Paths[1] != "docs/*.md" || s.Context != "fork" {
		t.Fatalf("list fields: tools=%v paths=%v ctx=%q", s.AllowedTools, s.Paths, s.Context)
	}
}

func TestParseSkillDir_AgentskillsWarnings(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my-skill")
	long := strings.Repeat("x", 1025)
	writeFile(t, filepath.Join(dir, "SKILL.md"), "---\nname: My--Skill\ndescription: "+long+"\nmetadata: [a, b]\nexec_mode: tool\n---\nbody\n")
	s, ds := ParseSkillDir(dir)
	if s == nil {
		t.Fatalf("warnings must not block loading: %v", ds)
	}
	for _, rule := range []string{RuleSkillNameCharset, RuleSkillNameDir, RuleSkillDescLength, RuleSkillMetadataShape, RuleSkillLegacyPolaris} {
		if !hasRule(ds, rule, SeverityWarning) {
			t.Errorf("expected warning %s, got %v", rule, ds)
		}
	}
}

func TestParseSkillDir_HardErrors(t *testing.T) {
	cases := map[string]string{
		"unclosed-frontmatter": "---\nname: a\n",
		"invalid-yaml":         "---\nname: [unclosed\n---\n",
		"name-with-slash":      "---\nname: a/b\ndescription: d\n---\n",
		"name-with-space":      "---\nname: a b\ndescription: d\n---\n",
		"name-with-dotdot":     "---\nname: ..evil\ndescription: d\n---\n",
		"name-too-long":        "---\nname: " + strings.Repeat("a", 65) + "\ndescription: d\n---\n",
		"name-bidi-control":    "---\nname: \"ab\u202Ecd\"\ndescription: d\n---\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "s")
			writeFile(t, filepath.Join(dir, "SKILL.md"), content)
			s, ds := ParseSkillDir(dir)
			if s != nil {
				t.Fatalf("expected hard error, got skill %+v", s)
			}
			if len(ds) == 0 || ds[0].Severity != SeverityError {
				t.Fatalf("expected error diagnostic, got %v", ds)
			}
		})
	}
}

func TestParseSkillDir_CodexOpenAIYAML(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "docs")
	writeFile(t, filepath.Join(dir, "SKILL.md"), "---\nname: docs\ndescription: d\n---\n")
	writeFile(t, filepath.Join(dir, "agents", "openai.yaml"), `interface:
  display_name: "Docs"
  default_prompt: "Look up docs"
policy:
  allow_implicit_invocation: false
dependencies:
  tools:
    - type: "mcp"
      value: "openaiDeveloperDocs"
      transport: "streamable_http"
      url: "https://developers.openai.com/mcp"
`)
	s, ds := ParseSkillDir(dir)
	if s == nil || len(ds) != 0 {
		t.Fatalf("skill=%v diags=%v", s, ds)
	}
	if s.OpenAI == nil || s.OpenAI.Interface.DisplayName != "Docs" || len(s.OpenAI.Dependencies.Tools) != 1 {
		t.Fatalf("openai.yaml not parsed: %+v", s.OpenAI)
	}
	if s.ModelInvocable() {
		t.Fatalf("allow_implicit_invocation=false must disable model invocation")
	}
}

func TestQualifiedName(t *testing.T) {
	if got := QualifiedName("deploy-tools", "review"); got != "deploy-tools:review" {
		t.Fatalf("got %q", got)
	}
	if got := QualifiedName("deploy-tools", "deploy-tools:review"); got != "deploy-tools:review" {
		t.Fatalf("prefix must not be doubled, got %q", got)
	}
}
