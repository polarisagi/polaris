package pluginspec

import (
	"reflect"
	"testing"
)

func TestVars_Expand(t *testing.T) {
	v := Vars{
		PluginRoot: "/p", PluginData: "/d",
		UserConfig: map[string]string{"api_token": "tok"},
		HostEnv:    map[string]string{"HOME": "/home/u"},
	}
	cases := []struct {
		in, want   string
		unresolved []string
	}{
		{"${CLAUDE_PLUGIN_ROOT}/server.js", "/p/server.js", nil},
		{"${PLUGIN_DATA}/cache", "/d/cache", nil},
		{"Bearer ${user_config.api_token}", "Bearer tok", nil},
		{"${HOME}/x", "/home/u/x", nil},
		// 宿主密钥不在白名单：视为未定义，不得泄漏。
		{"${OPENAI_API_KEY}", "", []string{"OPENAI_API_KEY"}},
		{"${OPENAI_API_KEY:-none}", "none", nil},
		{"${user_config.missing:-}", "", nil},
		// 非递归：值中的 ${...} 不再展开。
		{"${user_config.api_token}", "tok", nil},
	}
	for _, c := range cases {
		got, unresolved := v.Expand(c.in)
		if got != c.want || !reflect.DeepEqual(unresolved, c.unresolved) {
			t.Errorf("Expand(%q) = %q %v, want %q %v", c.in, got, unresolved, c.want, c.unresolved)
		}
	}
	v.UserConfig["nested"] = "${CLAUDE_PLUGIN_ROOT}"
	if got, _ := v.Expand("${user_config.nested}"); got != "${CLAUDE_PLUGIN_ROOT}" {
		t.Errorf("expansion must be non-recursive, got %q", got)
	}
}

func TestVars_ProcessEnv(t *testing.T) {
	env := Vars{PluginRoot: "/p", PluginData: "/d"}.ProcessEnv()
	for _, k := range []string{"PLUGIN_ROOT", "CLAUDE_PLUGIN_ROOT", "PLUGIN_DATA", "CLAUDE_PLUGIN_DATA"} {
		if env[k] == "" {
			t.Errorf("missing %s", k)
		}
	}
}
