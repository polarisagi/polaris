package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/token"
	"github.com/polarisagi/polaris/internal/tool"
	"github.com/polarisagi/polaris/internal/tool/builtin/web_fetch"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func TestDevelopmentWorkflowThroughExecuteTool(t *testing.T) {
	root := t.TempDir()
	setGitTestEnv(t, root)
	runGitStatusTest(t, root, "init", "-b", "main")
	runGitStatusTest(t, root, "config", "user.email", "test@example.invalid")
	runGitStatusTest(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/workflow\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package workflow\n\nfunc Value() string { return \"old\" }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGitStatusTest(t, root, "add", "go.mod", "main.go")
	runGitStatusTest(t, root, "commit", "-m", "initial")

	sbx := sandbox.NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	reg := tool.NewInMemoryToolRegistry(newBuiltinTestEnvelope(sbx, dummyTokenVerifier{}), config.DefaultThresholds().M7Tool)
	if err := regBuiltin(sbx, reg, []string{root}, nil, web_fetch.Deps{}); err != nil {
		t.Fatalf("RegisterBuiltinTools: %v", err)
	}
	ctx := context.WithValue(context.Background(), protocol.CtxCapabilityTokenKey{}, &token.Token{})
	call := func(name string, args map[string]any) *types.ToolResult {
		t.Helper()
		result, err := reg.ExecuteTool(ctx, name, mustJSON(t, args), types.TaintNone)
		if err != nil {
			t.Fatalf("ExecuteTool %s: %v", name, err)
		}
		if !result.Success {
			t.Fatalf("ExecuteTool %s failed: %s", name, result.Error)
		}
		return result
	}

	call("write_file", map[string]any{"path": filepath.Join(root, "main.go"), "content": "package workflow\n\nfunc Value() string { return }\n"})
	call("write_file", map[string]any{
		"path":    filepath.Join(root, "main_test.go"),
		"content": "package workflow\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { if Value() != \"new-value\" { t.Fatalf(\"Value() = %q\", Value()) } }\n",
	})
	time.Sleep(600 * time.Millisecond)
	failure := call("run_command", map[string]any{"command": "go test ./...", "working_dir": root, "timeout_s": 30})
	var failedBuild struct {
		ExitCode int    `json:"exit_code"`
		Error    string `json:"error"`
		Output   string `json:"output"`
	}
	if err := json.Unmarshal(failure.Output, &failedBuild); err != nil {
		t.Fatalf("failed build result is not structured: %v", err)
	}
	if failedBuild.ExitCode == 0 || (failedBuild.Error == "" && failedBuild.Output == "") {
		t.Fatalf("failed build output was not identifiable: %+v", failedBuild)
	}

	call("str_replace_editor", map[string]any{
		"command": "str_replace", "path": filepath.Join(root, "main.go"),
		"old_str": "func Value() string { return }", "new_str": "func Value() string { return \"new-value\" }",
	})
	search := call("grep", map[string]any{"pattern": "new-value", "path": root, "output_mode": "content"})
	if !strings.Contains(string(search.Output), "new-value") {
		t.Fatalf("grep missed edited source: %s", search.Output)
	}
	time.Sleep(600 * time.Millisecond)
	build := call("run_command", map[string]any{"command": "go test ./...", "working_dir": root, "timeout_s": 30})
	var passedBuild struct {
		ExitCode int `json:"exit_code"`
	}
	if err := json.Unmarshal(build.Output, &passedBuild); err != nil || passedBuild.ExitCode != 0 {
		t.Fatalf("build did not succeed: output=%s err=%v", build.Output, err)
	}
	time.Sleep(600 * time.Millisecond)
	status := call("git_status", map[string]any{"path": root})
	if !strings.Contains(string(status.Output), "main.go") {
		t.Fatalf("git_status missed changed source: %s", status.Output)
	}
	time.Sleep(600 * time.Millisecond)
	diff := call("git_diff", map[string]any{"path": root, "file": "main.go"})
	var diffResult struct {
		Raw string `json:"raw"`
	}
	if err := json.Unmarshal(diff.Output, &diffResult); err != nil || !strings.Contains(diffResult.Raw, "+func Value() string { return \"new-value\" }") {
		t.Fatalf("git_diff missed final edit: raw=%s err=%v", diffResult.Raw, err)
	}
	time.Sleep(600 * time.Millisecond)
	denied, err := reg.ExecuteTool(ctx, "git_status", mustJSON(t, map[string]string{"path": filepath.Dir(root)}), types.TaintNone)
	if err != nil {
		if apperr.CodeOf(err) != apperr.CodeForbidden {
			t.Fatalf("root escape rejected for unrelated reason: %v", err)
		}
	} else if denied == nil || denied.Success || !strings.Contains(strings.ToLower(denied.Error), "path") {
		t.Fatalf("git_status did not report path denial: result=%+v", denied)
	}

	meta, err := tool.GetBuiltinToolMeta("git_status")
	if err != nil || meta.Capability != types.CapReadOnly || meta.RiskLevel != types.RiskLow {
		t.Fatalf("git_status metadata invalid: %+v err=%v", meta, err)
	}
}
