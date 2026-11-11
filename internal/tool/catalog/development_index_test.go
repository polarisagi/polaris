package catalog

import (
	"strings"
	"testing"
)

func TestDevelopmentToolsAreDiscoverable(t *testing.T) {
	for _, name := range []string{"file_edit", "project_diagnostics", "project_preview", "code_patch_test"} {
		if IndexBrief(name) == "" {
			t.Errorf("%s has no lazy index brief", name)
		}
		if !strings.Contains(SearchAliases(name), "开发") {
			t.Errorf("%s is missing the development search alias: %q", name, SearchAliases(name))
		}
		if got := toolCategory(name); got != "开发" {
			t.Errorf("%s category=%q, want 开发", name, got)
		}
	}
}
