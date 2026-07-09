package plugin

import (
	"testing"
)

func TestSandboxLevel(t *testing.T) {
	if sandboxLevel("L3") != 3 {
		t.Errorf("L3 should be 3")
	}
	if sandboxLevel("l2") != 2 {
		t.Errorf("l2 should be 2")
	}
	if sandboxLevel("L1") != 1 {
		t.Errorf("L1 should be 1")
	}
	if sandboxLevel("unknown") != 1 {
		t.Errorf("unknown should fallback to 1")
	}
}

func TestFormatName(t *testing.T) {
	if res := formatName("my-cool-skill"); res != "My Cool Skill" {
		t.Errorf("expected My Cool Skill, got %s", res)
	}
	if res := formatName("a-b"); res != "A B" {
		t.Errorf("expected A B, got %s", res)
	}
}

func TestParseFrontmatter(t *testing.T) {
	content := `---
name: "Test Skill"
version: "1.2.3"
---
some other content
`
	fm := parseFrontmatter(content)
	if fm.Name != "Test Skill" {
		t.Errorf("expected Test Skill")
	}
	if fm.Version != "1.2.3" {
		t.Errorf("expected 1.2.3")
	}
	if fm.ExecMode != "tool" {
		t.Errorf("expected default tool")
	}
}

func TestParseSkillMD(t *testing.T) {
	content := `---
name: "test"
description: "desc"
tags: ["a"]
---`
	name, desc, tags, exec := parseSkillMD(content)
	if name != "test" {
		t.Errorf("expected test")
	}
	if desc != "desc" {
		t.Errorf("expected desc")
	}
	if len(tags) != 1 || tags[0] != "a" {
		t.Errorf("expected [a]")
	}
	if exec != "tool" {
		t.Errorf("expected tool")
	}
}
