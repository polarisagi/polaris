package lifecycle

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/polarisagi/polaris/internal/extension/pluginspec"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func installTestPlugin(t *testing.T, extRepo *testExtRepo, inst *PluginInstaller, instID, manifest string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), instID)
	writeTestFile(t, filepath.Join(root, ".claude-plugin", "plugin.json"), manifest)
	writeTestFile(t, filepath.Join(root, "skills", "s", "SKILL.md"), "---\nname: s\ndescription: d\n---\nbody")
	if _, err := inst.Install(context.Background(), InstallReq{InstID: instID, LocalPath: root}); err != nil {
		t.Fatal(err)
	}
}

func pluginEnabled(t *testing.T, extRepo *testExtRepo, id string) bool {
	t.Helper()
	rows, err := extRepo.ListPlugins(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == id {
			return r.Enabled
		}
	}
	t.Fatalf("plugin %s not found", id)
	return false
}

func TestPluginDependencies_LoadTimeSemantics(t *testing.T) {
	extRepo := newTestExtRepo(t)
	ctx := context.Background()
	inst := NewPluginInstaller(extRepo, &recordingConnector{}, &recordingSkillRegistry{}).WithPolicyGate(allowAllGate{})
	deps := NewPluginDependencies(extRepo, nil)

	// 依赖缺失：安装完成但插件不启用。
	installTestPlugin(t, extRepo, inst, "ext_kit", `{"name":"kit","dependencies":[{"name":"vault","version":"~2.1.0"},"logger"]}`)
	if pluginEnabled(t, extRepo, "pl_kit") {
		t.Fatal("plugin with missing dependencies must install disabled")
	}
	installTestPlugin(t, extRepo, inst, "ext_vault", `{"name":"vault","version":"2.2.0"}`)
	installTestPlugin(t, extRepo, inst, "ext_logger", `{"name":"logger"}`)
	st, err := deps.Check(ctx, "pl_kit")
	if err != nil || st[0].State != DepVersionMismatch || st[0].Message != `Requires "vault" ~2.1.0, installed 2.2.0` || st[1].State != DepOK {
		t.Fatalf("check: %+v %v", st, err)
	}
	if err := deps.EnableBlocker(ctx, "pl_kit"); !apperr.IsCode(err, apperr.CodeConflict) {
		t.Fatalf("enable must be blocked: %v", err)
	}

	installTestPlugin(t, extRepo, inst, "ext_vault", `{"name":"vault","version":"2.1.5"}`) // 升级到满足范围的版本
	installTestPlugin(t, extRepo, inst, "ext_kit", `{"name":"kit","dependencies":[{"name":"vault","version":"~2.1.0"},"logger"]}`)
	if !pluginEnabled(t, extRepo, "pl_kit") || deps.EnableBlocker(ctx, "pl_kit") != nil {
		t.Fatal("satisfied dependencies must allow enabling")
	}

	// 停用依赖：依赖方级联停用（链式传播到不动点）。
	installTestPlugin(t, extRepo, inst, "ext_top", `{"name":"top","dependencies":["kit"]}`)
	if err := extRepo.UpdatePluginStatus(ctx, "pl_logger", 0, "{}", "now"); err != nil {
		t.Fatal(err)
	}
	disabled, err := deps.EnforceAll(ctx)
	if err != nil || len(disabled) != 2 || pluginEnabled(t, extRepo, "pl_kit") || pluginEnabled(t, extRepo, "pl_top") {
		t.Fatalf("cascade: %v %v", disabled, err)
	}
}

func TestVersionState_PrereleaseAndInvalid(t *testing.T) {
	if st, _ := versionState(pluginDep("^2.0"), "2.1.0-beta.1"); st != DepVersionMismatch {
		t.Fatal("ranges must not match pre-releases without opting in")
	}
	if st, _ := versionState(pluginDep("^2.0.0-0"), "2.1.0-beta.1"); st != DepOK {
		t.Fatal("pre-release suffix in the range opts in")
	}
	if st, _ := versionState(pluginDep("not a range"), "1.0.0"); st != DepBadConstraint {
		t.Fatal("invalid range must be reported")
	}
}

func pluginDep(version string) pluginspec.Dependency {
	return pluginspec.Dependency{Name: "x", Version: version}
}
