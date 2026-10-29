package lifecycle

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/action/hook"
)

func TestHookSourceProvider_PluginAndUserSources(t *testing.T) {
	extRepo := newTestExtRepo(t)
	dataDir := t.TempDir()
	root := filepath.Join(t.TempDir(), "guard")
	writeTestFile(t, filepath.Join(root, ".claude-plugin", "plugin.json"), `{"name":"guard"}`)
	writeTestFile(t, filepath.Join(root, "hooks", "hooks.json"),
		`{"hooks":{"PreToolUse":[{"matcher":"bash","hooks":[{"type":"command","command":"${CLAUDE_PLUGIN_ROOT}/check.sh"}]}]}}`)
	inst := NewPluginInstaller(extRepo, &recordingConnector{}, &recordingSkillRegistry{}).WithPolicyGate(allowAllGate{})
	if _, err := inst.Install(context.Background(), InstallReq{InstID: "ext_g", LocalPath: root}); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, UserHooksPath(dataDir), `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"true"}]}]}}`)

	provider := NewHookSourceProvider(extRepo, dataDir, nil, nil)
	sources, err := provider.ListHookSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var plugin, user *hook.Source
	for i := range sources {
		switch sources[i].Scope {
		case hook.ScopePlugin:
			plugin = &sources[i]
		case hook.ScopeUser:
			user = &sources[i]
		}
	}
	if user == nil || !user.Trusted {
		t.Fatalf("user hooks must load trusted: %+v", sources)
	}
	if plugin == nil || plugin.Trusted || plugin.Key != "plugin:pl_g:hooks/hooks.json" || plugin.PluginRoot != root {
		t.Fatalf("plugin source: %+v", plugin)
	}
	if err := extRepo.SaveHookTrust(context.Background(), plugin.Key, plugin.Digest); err != nil {
		t.Fatal(err)
	}
	sources, _ = provider.ListHookSources(context.Background())
	for _, s := range sources {
		if s.Scope == hook.ScopePlugin && !s.Trusted {
			t.Fatal("plugin source must be trusted after digest review")
		}
	}
	// 卸载清除信任：重装须重新审阅。
	if err := inst.Uninstall(context.Background(), UninstallReq{InstID: "ext_g", RuntimeID: "pl_g"}); err != nil {
		t.Fatal(err)
	}
	if trust, _ := extRepo.ListHookTrust(context.Background()); len(trust) != 0 {
		t.Fatalf("hook trust must be removed with the plugin: %v", trust)
	}
}
