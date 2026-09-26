package pluginspec

import (
	"reflect"
	"testing"
)

func TestSplitShellArgs(t *testing.T) {
	got := splitShellArgs(`"hello world" second 'a b' c\ d`)
	want := []string{"hello world", "second", "a b", "c d"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestRenderSkill(t *testing.T) {
	in := RenderInput{
		RawArgs: `"hello world" second`, ArgNames: []string{"greeting", "who", "missing"},
		SkillDir: "/s", PluginRoot: "/p", SessionID: "sess1",
		UserConfig: map[string]string{"tone": "warm"}, SensitiveKeys: map[string]bool{"token": true},
	}
	cases := []struct{ body, want string }{
		{"All: $ARGUMENTS", `All: "hello world" second`},
		{"$0|$1|$ARGUMENTS[1]|$5", "hello world|second|second|$5"},
		{"$greeting to $who ($missing)", "hello world to second ()"},
		{"cost \\$1.00 and \\\\$1", "cost $1.00 and \\\\second"},
		{"${CLAUDE_SKILL_DIR}/scripts/x.py ${CLAUDE_PLUGIN_ROOT} ${CLAUDE_SESSION_ID} $0", "/s/scripts/x.py /p sess1 hello world"},
		{"tone=${user_config.tone} token=${user_config.token} unknown=${user_config.nope} $1", "tone=warm token=<sensitive:token> unknown=${user_config.nope} second"},
		{"shell keeps $HOME and ${HOME}", "shell keeps $HOME and ${HOME}\n\nARGUMENTS: \"hello world\" second\n"},
	}
	for _, c := range cases {
		if got := RenderSkill(c.body, in); got != c.want {
			t.Errorf("RenderSkill(%q)\n got  %q\n want %q", c.body, got, c.want)
		}
	}
}

// 参数值按字面插入：值里的 "$1" / "${...}" 不得被二次展开。
func TestRenderSkill_ArgumentsInsertedLiterally(t *testing.T) {
	in := RenderInput{RawArgs: `'$1 ${CLAUDE_SKILL_DIR}'`, SkillDir: "/s"}
	if got := RenderSkill("x=$0", in); got != "x=$1 ${CLAUDE_SKILL_DIR}" {
		t.Fatalf("got %q", got)
	}
	if got := RenderSkill("no placeholders", RenderInput{}); got != "no placeholders" {
		t.Fatalf("no args must not append ARGUMENTS: %q", got)
	}
}
