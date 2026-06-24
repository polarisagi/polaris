package builtin

import (
	"github.com/polarisagi/polaris/internal/security/token"

	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/polarisagi/polaris/internal/tool"
	toolsb "github.com/polarisagi/polaris/internal/tool/sandbox"

	"github.com/polarisagi/polaris/internal/action"
	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
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

type mockPolicy struct{}

func (mockPolicy) IsAuthorized(ctx context.Context, principal, action, resource string, contextData map[string]any) (bool, error) {
	return true, nil
}

func (mockPolicy) Review(ctx context.Context, req types.PolicyReviewRequest) (types.PolicyReviewResult, error) {
	return types.PolicyReviewResult{Allowed: true}, nil
}

func mockToken(caps []token.CapabilityType) *token.Token {
	tok, _ := action.GetTokenManager().Mint("agent1", caps, 1, 5*time.Minute, 0)
	return tok
}

// TestBuiltinTools_ReadFile_AllowedPath 验证 read_file 在白名单路径下能读取真实文件。
func TestBuiltinTools_ReadFile_AllowedPath(t *testing.T) {
	tmpDir := t.TempDir()
	sbx := sandbox.NewInProcessSandbox()
	toolReg := tool.NewInMemoryToolRegistry(sandbox.NewExecEnvelope(dummyPolicyGate{}, sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0), 0, "", nil)) // 无 PolicyGate，只测工具逻辑
	if err := RegisterBuiltinTools(sbx, toolReg, []string{tmpDir}, dummyDialerPtr, false, toolsb.NetworkBlock, "", &config.Config{}, nil); err != nil {
		t.Fatalf("RegisterBuiltinTools: %v", err)
	}

	// 创建临时文件
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("hello polaris"), 0o600); err != nil {
		t.Fatal(err)
	}

	args, _ := json.Marshal(map[string]string{"path": testFile})
	tok := mockToken([]token.CapabilityType{token.CapFS})
	ctx := context.WithValue(context.Background(), protocol.CtxCapabilityToken{}, tok)
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
	sbx := sandbox.NewInProcessSandbox()
	toolReg := tool.NewInMemoryToolRegistry(sandbox.NewExecEnvelope(dummyPolicyGate{}, sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0), 0, "", nil))
	if err := RegisterBuiltinTools(sbx, toolReg, []string{tmpDir}, dummyDialerPtr, false, toolsb.NetworkBlock, "", &config.Config{}, nil); err != nil {
		t.Fatalf("RegisterBuiltinTools: %v", err)
	}

	args, _ := json.Marshal(map[string]string{"path": "/etc/passwd"})
	tok := mockToken([]token.CapabilityType{token.CapFS})
	ctx := context.WithValue(context.Background(), protocol.CtxCapabilityToken{}, tok)
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
	sbx := sandbox.NewInProcessSandbox()
	toolReg := tool.NewInMemoryToolRegistry(sandbox.NewExecEnvelope(dummyPolicyGate{}, sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0), 0, "", nil))
	if err := RegisterBuiltinTools(sbx, toolReg, []string{tmpDir}, dummyDialerPtr, false, toolsb.NetworkBlock, "", &config.Config{}, nil); err != nil {
		t.Fatalf("RegisterBuiltinTools: %v", err)
	}

	// 创建两个文件
	os.WriteFile(filepath.Join(tmpDir, "a.txt"), []byte("a"), 0o600)
	os.WriteFile(filepath.Join(tmpDir, "b.txt"), []byte("b"), 0o600)

	args, _ := json.Marshal(map[string]string{"path": tmpDir})
	tok := mockToken([]token.CapabilityType{token.CapFS})
	ctx := context.WithValue(context.Background(), protocol.CtxCapabilityToken{}, tok)
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
	sbx := sandbox.NewInProcessSandbox()
	toolReg := tool.NewInMemoryToolRegistry(sandbox.NewExecEnvelope(dummyPolicyGate{}, sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0), 0, "", nil))
	if err := RegisterBuiltinTools(sbx, toolReg, []string{tmpDir}, dummyDialerPtr, false, toolsb.NetworkBlock, "", &config.Config{}, nil); err != nil {
		t.Fatalf("RegisterBuiltinTools: %v", err)
	}

	outFile := filepath.Join(tmpDir, "out.txt")
	args, _ := json.Marshal(map[string]any{
		"path":    outFile,
		"content": "written by agent",
		"append":  false,
	})
	tok := mockToken([]token.CapabilityType{token.CapFS})
	ctx := context.WithValue(context.Background(), protocol.CtxCapabilityToken{}, tok)
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
	sbx := sandbox.NewInProcessSandbox()
	toolReg := tool.NewInMemoryToolRegistry(sandbox.NewExecEnvelope(dummyPolicyGate{}, sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0), 0, "", nil))
	if err := RegisterBuiltinTools(sbx, toolReg, nil, dummyDialerPtr, false, toolsb.NetworkBlock, "", &config.Config{}, nil); err != nil {
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
		tok := mockToken([]token.CapabilityType{token.CapNetwork})
		ctx := context.WithValue(context.Background(), protocol.CtxCapabilityToken{}, tok)
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
	sbx := sandbox.NewInProcessSandbox()
	toolReg := tool.NewInMemoryToolRegistry(sandbox.NewExecEnvelope(dummyPolicyGate{}, sandbox.NewSandboxRouter(sbx, nil, nil, "linux", 0), 0, "", nil))
	if err := RegisterBuiltinTools(sbx, toolReg, nil, dummyDialerPtr, false, toolsb.NetworkBlock, "", &config.Config{}, nil); err != nil {
		t.Fatalf("RegisterBuiltinTools: %v", err)
	}

	t.Skip("Skipping network test to avoid flakiness")
}
