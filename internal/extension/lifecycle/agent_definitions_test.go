package lifecycle

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestAgentDefinitionProvider_SourcesAndResolve(t *testing.T) {
	extRepo := newTestExtRepo(t)
	dataDir, project := t.TempDir(), t.TempDir()
	root := filepath.Join(t.TempDir(), "review")
	writeTestFile(t, filepath.Join(root, ".claude-plugin", "plugin.json"), `{"name":"review"}`)
	writeTestFile(t, filepath.Join(root, "agents", "security.md"),
		"---\ndescription: Security reviewer\ntools: Read, Grep\nskills: checklist\nhooks: {}\n---\nFind vulnerabilities.")
	inst := NewPluginInstaller(extRepo, &recordingConnector{}, &recordingSkillRegistry{}).WithPolicyGate(allowAllGate{})
	if _, err := inst.Install(context.Background(), InstallReq{InstID: "ext_r", LocalPath: root}); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(UserAgentsDir(dataDir), "explorer.toml"),
		"name = \"explorer\"\ndescription = \"user explorer\"\ndeveloper_instructions = \"Explore.\"\nsandbox_mode = \"read-only\"\n")
	writeTestFile(t, filepath.Join(project, ".polaris", "agents", "explorer.md"), "---\ndescription: project explorer\n---\nProject rules.")

	var loaded []string
	provider := NewAgentDefinitionProvider(extRepo, dataDir, func() []string { return []string{project} },
		func(_ context.Context, name string) (string, error) {
			loaded = append(loaded, name)
			if name == PluginSkillName("review", "checklist") {
				return "CHECKLIST", nil
			}
			return "", apperr.New(apperr.CodeNotFound, name)
		})
	defs, _, err := provider.ListAgentDefinitions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]AgentDefinition{}
	for _, d := range defs {
		byName[d.Name] = d
	}
	if len(defs) != 2+len(builtinAgents())-1 || byName["explorer"].Source != "project" || byName["Explore"].Source != "builtin" ||
		byName["review:security"].Source != "plugin:pl_r" || len(byName["review:security"].NotApplied) != 1 {
		t.Fatalf("project must shadow user and builtin explorer; plugin agent namespaced: %+v", defs)
	}
	if spec, err := provider.ResolveAgentProfile(context.Background(), "Explore"); err != nil || !spec.ReadOnly || spec.InstructionTaint != types.TaintLow {
		t.Fatalf("builtin Explore: %+v %v", spec, err)
	}

	spec, err := provider.ResolveAgentProfile(context.Background(), "review:security")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(spec.Instructions, "Find vulnerabilities.") || !strings.Contains(spec.Instructions, "CHECKLIST") ||
		spec.InstructionTaint != types.TaintMedium || len(spec.Tools) != 2 || spec.AllowDelegation {
		t.Fatalf("plugin agent spec: %+v", spec)
	}
	if len(loaded) != 1 || loaded[0] != PluginSkillName("review", "checklist") {
		t.Fatalf("plugin agent skills resolve in plugin namespace first: %v", loaded)
	}
	if _, err := provider.ResolveAgentProfile(context.Background(), "nope"); !apperr.IsCode(err, apperr.CodeNotFound) {
		t.Fatalf("unknown agent must be NotFound, got %v", err)
	}
}

func TestAgentDefinitionProvider_MissingPreloadSkillFails(t *testing.T) {
	dataDir := t.TempDir()
	writeTestFile(t, filepath.Join(UserAgentsDir(dataDir), "a.md"), "---\ndescription: d\nskills: [missing]\n---\nbody")
	provider := NewAgentDefinitionProvider(newTestExtRepo(t), dataDir, nil, func(_ context.Context, name string) (string, error) {
		return "", apperr.New(apperr.CodeNotFound, name)
	})
	spec, err := provider.ResolveAgentProfile(context.Background(), "a")
	if err == nil || spec != nil {
		t.Fatalf("missing preload skill must fail instead of running without it: %v", err)
	}
	if spec, err := NewAgentDefinitionProvider(newTestExtRepo(t), t.TempDir(), nil, nil).ResolveAgentProfile(context.Background(), "a"); err == nil || spec != nil {
		t.Fatal("agent from another data dir must not resolve")
	}
}
