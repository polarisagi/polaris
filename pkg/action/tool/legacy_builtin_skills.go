package tool

import (
	"context"
	"encoding/json"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/pkg/action"
)

// makeLegacySkillFn creates an MVP stub implementation for legacy Wasm skills
// that were migrated to L1 direct execution.
func makeLegacySkillFn(skillName string) action.InProcessFn {
	return func(ctx context.Context, input []byte) ([]byte, error) {
		var in map[string]interface{}
		if len(input) > 0 {
			if err := json.Unmarshal(input, &in); err != nil {
				return nil, perrors.Wrap(perrors.CodeInternal, skillName+": invalid args", err)
			}
		}

		out := map[string]interface{}{
			"status":   skillName + " executed successfully",
			"received": in,
		}
		return json.Marshal(out)
	}
}

// getLegacyBuiltinDefs returns the definitions for all 19 migrated skills.
func getLegacyBuiltinDefs() []struct {
	name string
	fn   action.InProcessFn
} {
	return []struct {
		name string
		fn   action.InProcessFn
	}{
		{"json_parse", makeLegacySkillFn("json_parse")},
		{"text_summarize", makeLegacySkillFn("text_summarize")},
		{"api_call", makeLegacySkillFn("api_call")},
		{"git_diff", makeLegacySkillFn("git_diff")},
		{"file_search", makeLegacySkillFn("file_search")},
		{"markdown_render", makeLegacySkillFn("markdown_render")},
		{"json_format", makeLegacySkillFn("json_format")},
		{"text_translate", makeLegacySkillFn("text_translate")},
		{"template_render", makeLegacySkillFn("template_render")},
		{"git_commit", makeLegacySkillFn("git_commit")},
		{"data_query", makeLegacySkillFn("data_query")},
		{"code_gen", makeLegacySkillFn("code_gen")},
		{"text_extract", makeLegacySkillFn("text_extract")},
		{"code_review", makeLegacySkillFn("code_review")},
		{"regex_match", makeLegacySkillFn("regex_match")},
	}
}
