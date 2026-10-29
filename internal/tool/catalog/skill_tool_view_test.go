package catalog

import (
	"strings"
	"testing"

	"github.com/polarisagi/polaris/pkg/types"
)

func TestModelToolView(t *testing.T) {
	meta := types.SkillMeta{
		Name:        "skill:deploy-tools__deploy",
		Description: "Deploy the service.",
		DisplayName: "deploy-tools:deploy",
		Spec:        `{"when_to_use":"Use when the user asks to ship.","argument_hint":"[env]"}`,
	}
	view, ok := ModelToolView(meta)
	if !ok || view.Name != "skill__deploy-tools__deploy" {
		t.Fatalf("view=%+v ok=%v", view, ok)
	}
	if view.Description != "[deploy-tools:deploy] Deploy the service. Use when the user asks to ship." {
		t.Fatalf("description: %q", view.Description)
	}
	props := view.InputSchema["properties"].(map[string]any)["arguments"].(map[string]any)
	if !strings.Contains(props["description"].(string), "[env]") {
		t.Fatalf("argument hint missing: %v", props)
	}

	for name, m := range map[string]types.SkillMeta{
		"model-disabled": {Name: "skill:x", DisableModelInvocation: true},
		"deprecated":     {Name: "skill:x", Deprecated: true},
		"bad-prefix":     {Name: "x"},
	} {
		if _, ok := ModelToolView(m); ok {
			t.Errorf("%s must not be exposed to the model", name)
		}
	}
}

func TestModelToolView_TruncatesListing(t *testing.T) {
	view, _ := ModelToolView(types.SkillMeta{Name: "skill:x", Description: strings.Repeat("a", 2000)})
	if n := len([]rune(view.Description)); n != skillListingMaxRunes {
		t.Fatalf("listing must be capped at %d runes, got %d", skillListingMaxRunes, n)
	}
}

// 旧写入方（学习技能等）只有 capabilities 中的 description: 条目。
func TestModelToolView_CapabilityFallback(t *testing.T) {
	view, _ := ModelToolView(types.SkillMeta{Name: "skill:x", Capabilities: []string{"description:learned"}})
	if view.Description != "learned" {
		t.Fatalf("got %q", view.Description)
	}
}
