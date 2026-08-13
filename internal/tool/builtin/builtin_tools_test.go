package builtin

import (
	"fmt"

	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/tool"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/token"
	"github.com/polarisagi/polaris/internal/tool/builtin/read_tool_ref"
	"github.com/polarisagi/polaris/pkg/types"
)

type dummyDialer struct {
	net.Dialer
}

var dummyDialerPtr = &dummyDialer{}

type dummyPolicyGate struct{}

func (dummyPolicyGate) IsAuthorized(_ context.Context, _, _, _ string, _ map[string]any) (bool, error) {
	return true, nil
}
func (dummyPolicyGate) Review(_ context.Context, _ types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: true}, nil
}

type dummyTokenVerifier struct{}

func (dummyTokenVerifier) Verify(_ *token.Token) error { return nil }

// TestBuiltinTools_ReadFile_AllowedPath 验证 read_file 在白名单路径下能读取真实文件。
func TestBuiltinTools_ReadFile_AllowedPath(t *testing.T) {
	tmpDir := t.TempDir()
	sbx := sandbox.NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	toolReg := tool.NewInMemoryToolRegistry(sandbox.NewExecEnvelope(dummyPolicyGate{}, sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0), 0, "", nil), config.DefaultThresholds().M7Tool) // 无 PolicyGate，只测工具逻辑
	if err := RegisterBuiltinTools(sbx, toolReg, []string{tmpDir}, dummyDialerPtr, false, protocol.NetPolicyDeny, "", &config.Config{}, nil, "", nil, nil); err != nil {
		t.Fatalf("RegisterBuiltinTools: %v", err)
	}

	// 创建临时文件
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("hello polaris"), 0o600); err != nil {
		t.Fatal(err)
	}

	args, _ := json.Marshal(map[string]string{"path": testFile})
	ctx := context.Background()
	result, err := toolReg.ExecuteTool(ctx, "read_file", args, types.TaintNone)
	if err != nil {
		t.Fatalf("ExecuteTool read_file: %v", err)
	}
	if !result.Success {
		t.Fatalf("read_file failed: %s", result.Error)
	}
	if string(result.Output) != "hello polaris" {
		t.Errorf("expected 'hello polaris', got %q", result.Output)
	}
}

// TestBuiltinTools_ReadFile_BlockedPath 验证 read_file 拒绝白名单外路径。
func TestBuiltinTools_ReadFile_BlockedPath(t *testing.T) {
	tmpDir := t.TempDir()
	sbx := sandbox.NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	toolReg := tool.NewInMemoryToolRegistry(sandbox.NewExecEnvelope(dummyPolicyGate{}, sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0), 0, "", nil), config.DefaultThresholds().M7Tool)
	if err := RegisterBuiltinTools(sbx, toolReg, []string{tmpDir}, dummyDialerPtr, false, protocol.NetPolicyDeny, "", &config.Config{}, nil, "", nil, nil); err != nil {
		t.Fatalf("RegisterBuiltinTools: %v", err)
	}

	args, _ := json.Marshal(map[string]string{"path": "/etc/passwd"})
	ctx := context.Background()
	result, err := toolReg.ExecuteTool(ctx, "read_file", args, types.TaintNone)
	if err != nil {
		t.Fatalf("ExecuteTool should not return err: %v", err)
	}
	// PolicyGate 为 nil 时工具执行会通过 policy 阶段，但 path_guard 应拦截
	t.Logf("res=%+v", result)
	if result.Success {
		t.Error("read_file should fail for paths outside allowedPaths")
	}
}

// TestBuiltinTools_ListDir 验证 list_dir 能列举临时目录。
func TestBuiltinTools_ListDir(t *testing.T) {
	tmpDir := t.TempDir()
	sbx := sandbox.NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	toolReg := tool.NewInMemoryToolRegistry(sandbox.NewExecEnvelope(dummyPolicyGate{}, sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0), 0, "", nil), config.DefaultThresholds().M7Tool)
	if err := RegisterBuiltinTools(sbx, toolReg, []string{tmpDir}, dummyDialerPtr, false, protocol.NetPolicyDeny, "", &config.Config{}, nil, "", nil, nil); err != nil {
		t.Fatalf("RegisterBuiltinTools: %v", err)
	}

	// 创建两个文件
	os.WriteFile(filepath.Join(tmpDir, "a.txt"), []byte("a"), 0o600)
	os.WriteFile(filepath.Join(tmpDir, "b.txt"), []byte("b"), 0o600)

	args, _ := json.Marshal(map[string]string{"path": tmpDir})
	ctx := context.Background()
	result, err := toolReg.ExecuteTool(ctx, "list_dir", args, types.TaintNone)
	if err != nil {
		t.Fatalf("ExecuteTool list_dir: %v", err)
	}
	if !result.Success {
		t.Fatalf("list_dir failed: %s", result.Error)
	}

	var out struct {
		Entries []struct {
			Name string `json:"name"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(result.Output, &out); err != nil {
		t.Fatalf("list_dir output parse: %v", err)
	}
	if len(out.Entries) < 2 {
		t.Errorf("expected at least 2 entries, got %d", len(out.Entries))
	}
}

// TestBuiltinTools_WriteFile_AllowedPath 验证 write_file 在白名单路径下写文件。
func TestBuiltinTools_WriteFile_AllowedPath(t *testing.T) {
	tmpDir := t.TempDir()
	sbx := sandbox.NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	toolReg := tool.NewInMemoryToolRegistry(sandbox.NewExecEnvelope(dummyPolicyGate{}, sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0), 0, "", dummyTokenVerifier{}), config.DefaultThresholds().M7Tool)
	if err := RegisterBuiltinTools(sbx, toolReg, []string{tmpDir}, dummyDialerPtr, false, protocol.NetPolicyDeny, "", &config.Config{}, nil, "", nil, nil); err != nil {
		t.Fatalf("RegisterBuiltinTools: %v", err)
	}

	outFile := filepath.Join(tmpDir, "out.txt")
	args, _ := json.Marshal(map[string]any{
		"path":    outFile,
		"content": "written by agent",
		"append":  false,
	})
	ctx := context.WithValue(context.Background(), protocol.CtxCapabilityTokenKey{}, &token.Token{})
	result, err := toolReg.ExecuteTool(ctx, "write_file", args, types.TaintNone)
	if err != nil {
		t.Fatalf("ExecuteTool write_file: %v", err)
	}
	if !result.Success {
		t.Fatalf("write_file failed: %s", result.Error)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "written by agent" {
		t.Errorf("unexpected file content: %q", data)
	}
}

// TestBuiltinTools_FetchURL_SSRFGuard 验证 fetch_url 阻断私有地址。
func TestBuiltinTools_FetchURL_SSRFGuard(t *testing.T) {
	sbx := sandbox.NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	toolReg := tool.NewInMemoryToolRegistry(sandbox.NewExecEnvelope(dummyPolicyGate{}, sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0), 0, "", nil), config.DefaultThresholds().M7Tool)
	if err := RegisterBuiltinTools(sbx, toolReg, nil, dummyDialerPtr, false, protocol.NetPolicyDeny, "", &config.Config{}, nil, "", nil, nil); err != nil {
		t.Fatalf("RegisterBuiltinTools: %v", err)
	}

	blocked := []string{
		"http://localhost/",
		"http://127.0.0.1:8080/secret",
		"http://169.254.169.254/metadata",
		"http://192.168.1.1/admin",
	}
	for _, url := range blocked {
		args, _ := json.Marshal(map[string]string{"url": url})
		ctx := context.Background()
		result, err := toolReg.ExecuteTool(ctx, "fetch_url", args, types.TaintNone)
		if err != nil {
			t.Fatalf("ExecuteTool should not return err: %v", err)
		}
		t.Logf("res=%+v", result)
		if result.Success {
			t.Errorf("fetch_url should block private URL %q", url)
		}
	}
}

// TestBuiltinTools_FetchURL_PublicURL 验证 fetch_url 放行公共 URL（MVP stub 模式）。
func TestBuiltinTools_FetchURL_PublicURL(t *testing.T) {
	sbx := sandbox.NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	toolReg := tool.NewInMemoryToolRegistry(sandbox.NewExecEnvelope(dummyPolicyGate{}, sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0), 0, "", nil), config.DefaultThresholds().M7Tool)
	if err := RegisterBuiltinTools(sbx, toolReg, nil, dummyDialerPtr, false, protocol.NetPolicyDeny, "", &config.Config{}, nil, "", nil, nil); err != nil {
		t.Fatalf("RegisterBuiltinTools: %v", err)
	}

	t.Skip("Skipping network test to avoid flakiness")
}

// TestBuiltinTools_GitDiffAndCommit 覆盖 git_diff/git_commit 两个此前只有
// 空壳占位文件、后被 git_text_tools.go 实现但从未注册进 defs 的工具
// （GR-5-008，见 scripts/deadcode-allowlist.txt 历史记录）：验证注册后能
// 通过 ExecuteTool 正常调用真实 git 二进制，而不仅仅是"metadata 能加载"。
func TestBuiltinTools_GitDiffAndCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in test environment")
	}

	tmpDir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
	runGit("init")
	runGit("config", "user.email", "test@polarisagi.online")
	runGit("config", "user.name", "polaris-test")

	sbx := sandbox.NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	toolReg := tool.NewInMemoryToolRegistry(sandbox.NewExecEnvelope(dummyPolicyGate{}, sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0), 0, "", dummyTokenVerifier{}), config.DefaultThresholds().M7Tool)
	if err := RegisterBuiltinTools(sbx, toolReg, []string{tmpDir}, dummyDialerPtr, false, protocol.NetPolicyDeny, "", &config.Config{}, nil, "", nil, nil); err != nil {
		t.Fatalf("RegisterBuiltinTools: %v", err)
	}
	ctx := context.WithValue(context.Background(), protocol.CtxCapabilityTokenKey{}, &token.Token{})

	// 首次提交
	if err := os.WriteFile(filepath.Join(tmpDir, "a.txt"), []byte("v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commitArgs, _ := json.Marshal(map[string]any{"path": tmpDir, "message": "init"})
	res, err := toolReg.ExecuteTool(ctx, "git_commit", commitArgs, types.TaintNone)
	if err != nil {
		t.Fatalf("ExecuteTool git_commit: %v", err)
	}
	if !res.Success {
		t.Fatalf("git_commit (init) failed: %s", res.Error)
	}
	var commitOut struct {
		Hash   string `json:"hash"`
		Branch string `json:"branch"`
	}
	if err := json.Unmarshal(res.Output, &commitOut); err != nil {
		t.Fatalf("git_commit output parse: %v", err)
	}
	if commitOut.Hash == "" {
		t.Error("expected non-empty commit hash")
	}

	// git_diff/git_commit 都声明 SideProcessSpawn，共享 tool_registry.go 的
	// "shell" 令牌桶限速器（2 QPS，见 tool_helpers.go isShellTool 注释）。
	// 本测试连续发起 3 次 ExecuteTool 调用会撞上该限速，用短暂 sleep 让令牌桶
	// 补充——这是在验证真实系统边界，不是绕过它。
	time.Sleep(600 * time.Millisecond)

	// 修改文件后验证 git_diff 能看到变更
	if err := os.WriteFile(filepath.Join(tmpDir, "a.txt"), []byte("v1\nv2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	diffArgs, _ := json.Marshal(map[string]any{"path": tmpDir})
	res, err = toolReg.ExecuteTool(ctx, "git_diff", diffArgs, types.TaintNone)
	if err != nil {
		t.Fatalf("ExecuteTool git_diff: %v", err)
	}
	if !res.Success {
		t.Fatalf("git_diff failed: %s", res.Error)
	}
	var diffOut struct {
		Files []struct {
			Path         string `json:"path"`
			LinesAdded   int    `json:"lines_added"`
			LinesRemoved int    `json:"lines_removed"`
		} `json:"files"`
		Raw string `json:"raw"`
	}
	if err := json.Unmarshal(res.Output, &diffOut); err != nil {
		t.Fatalf("git_diff output parse: %v", err)
	}
	if len(diffOut.Files) != 1 || diffOut.Files[0].Path != "a.txt" || diffOut.Files[0].LinesAdded != 1 {
		t.Errorf("unexpected diff stats: %+v", diffOut.Files)
	}
	if !strings.Contains(diffOut.Raw, "+v2") {
		t.Errorf("expected raw diff to contain added line, got: %s", diffOut.Raw)
	}

	time.Sleep(600 * time.Millisecond)

	// 再次提交，验证第二次 commit 也能正常工作（且 hash 与首次不同）
	commitArgs2, _ := json.Marshal(map[string]any{"path": tmpDir, "message": "update", "files": []string{"a.txt"}})
	res, err = toolReg.ExecuteTool(ctx, "git_commit", commitArgs2, types.TaintNone)
	if err != nil {
		t.Fatalf("ExecuteTool git_commit (update): %v", err)
	}
	if !res.Success {
		t.Fatalf("git_commit (update) failed: %s", res.Error)
	}
	var commitOut2 struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal(res.Output, &commitOut2); err != nil {
		t.Fatalf("git_commit (update) output parse: %v", err)
	}
	if commitOut2.Hash == "" || commitOut2.Hash == commitOut.Hash {
		t.Errorf("expected a new distinct commit hash, got %q (previous %q)", commitOut2.Hash, commitOut.Hash)
	}
}

// TestBuiltinTools_TemplateRender 覆盖 template_render（此前同样只注册了
// metadata 但从未接入 defs 的工具）：验证 Go text/template 渲染与截断逻辑。
func TestBuiltinTools_TemplateRender(t *testing.T) {
	sbx := sandbox.NewInProcessSandbox(config.DefaultThresholds().M7Tool)
	toolReg := tool.NewInMemoryToolRegistry(sandbox.NewExecEnvelope(dummyPolicyGate{}, sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0), 0, "", nil), config.DefaultThresholds().M7Tool)
	if err := RegisterBuiltinTools(sbx, toolReg, nil, dummyDialerPtr, false, protocol.NetPolicyDeny, "", &config.Config{}, nil, "", nil, nil); err != nil {
		t.Fatalf("RegisterBuiltinTools: %v", err)
	}
	ctx := context.Background()

	args, _ := json.Marshal(map[string]any{
		"template": "Hello, {{.name}}! You have {{.count}} messages.",
		"data":     map[string]any{"name": "Polaris", "count": 3},
	})
	res, err := toolReg.ExecuteTool(ctx, "template_render", args, types.TaintNone)
	if err != nil {
		t.Fatalf("ExecuteTool template_render: %v", err)
	}
	if !res.Success {
		t.Fatalf("template_render failed: %s", res.Error)
	}
	var out struct {
		Output    string `json:"output"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal(res.Output, &out); err != nil {
		t.Fatalf("template_render output parse: %v", err)
	}
	if out.Output != "Hello, Polaris! You have 3 messages." {
		t.Errorf("unexpected render output: %q", out.Output)
	}
	if out.Truncated {
		t.Error("did not expect truncation for short output")
	}
}

func TestMakeReadToolRefFn(t *testing.T) {
	vfsRoot := t.TempDir()
	fn := read_tool_ref.MakeReadToolRefFn(vfsRoot)
	ctx := context.Background()

	// 1. Invalid args
	_, err := fn(ctx, []byte(`{"id": "123"}`)) // missing task_id
	if err == nil {
		t.Errorf("expected error for missing task_id")
	}

	// 2. Setup mock data
	taskID := "task-789"
	id := "mock-uuid"
	toolRefsDir := filepath.Join(vfsRoot, taskID, "tool_refs")
	if err := os.MkdirAll(toolRefsDir, 0700); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	content := "some large tool output"
	filePath := filepath.Join(toolRefsDir, id+".log")
	if err := os.WriteFile(filePath, []byte(content), 0600); err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	// 3. Successful read
	validArgs := fmt.Sprintf(`{"task_id": "%s", "id": "%s"}`, taskID, id)
	out, err := fn(ctx, []byte(validArgs))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != content {
		t.Errorf("content mismatch, got %q, want %q", string(out), content)
	}

	// 4. Path traversal prevention
	badArgs := fmt.Sprintf(`{"task_id": "%s", "id": "../../../../etc/passwd"}`, taskID)
	_, err = fn(ctx, []byte(badArgs))
	if err == nil {
		t.Errorf("expected error for missing file or traversal")
	}

	badTaskArgs := `{"task_id": "../system", "id": "123"}`
	_, err = fn(ctx, []byte(badTaskArgs))
	if err == nil {
		t.Errorf("expected error for path traversal in task_id")
	}
}
