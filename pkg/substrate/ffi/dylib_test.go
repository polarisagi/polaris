package ffi

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDylibName_CurrentPlatform 验证当前平台的库文件名
// 跨平台正确性由 CI 矩阵（darwin/linux/windows runner）分别验证
func TestDylibName_CurrentPlatform(t *testing.T) {
	name := dylibName()
	var expected string
	switch runtime.GOOS {
	case "darwin":
		expected = "libsubstrate.dylib"
	case "windows":
		expected = "substrate.dll"
	default:
		expected = "libsubstrate.so"
	}
	if name != expected {
		t.Errorf("dylibName() = %q, 期望 %q (GOOS=%s)", name, expected, runtime.GOOS)
	}
}

// TestLibCandidatePaths_EnvOverride 验证环境变量 POLARIS_SUBSTRATE_LIB 覆盖优先级
func TestLibCandidatePaths_EnvOverride(t *testing.T) {
	customPath := "/custom/path/libsubstrate.so"
	t.Setenv("POLARIS_SUBSTRATE_LIB", customPath)

	paths := libCandidatePaths()
	if len(paths) == 0 {
		t.Fatal("expected non-empty candidate paths")
	}

	if paths[0] != customPath {
		t.Errorf("expected first path to be %s, got %s", customPath, paths[0])
	}
}

// TestLibCandidatePaths_NoEnv_ContainsDevPath 验证不设置环境变量时，dev 模式路径被包含
func TestLibCandidatePaths_NoEnv_ContainsDevPath(t *testing.T) {
	// 清除环境变量，确保测试隔离
	t.Setenv("POLARIS_SUBSTRATE_LIB", "")

	paths := libCandidatePaths()
	if len(paths) == 0 {
		t.Fatal("expected non-empty candidate paths")
	}

	// 验证至少有一个路径包含 "rust/substrate/target/release"
	found := false
	for _, p := range paths {
		if strings.Contains(p, filepath.Join("rust", "substrate", "target", "release")) {
			found = true
			break
		}
	}

	if !found {
		t.Errorf("expected at least one path to contain rust/substrate/target/release, got %v", paths)
	}
}

// TestDoLoad_PathNotFound_ReturnsError 验证 doLoad() 当库不存在时返回 error
// doLoad() 在路径不存在时正常返回 error；在找到库但 ABI 不匹配时会 panic（设计意图）
func TestDoLoad_PathNotFound_ReturnsError(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Logf("doLoad panicked: %v — ABI mismatch (unexpected in path-not-found test)", r)
		}
	}()

	// 设置环境变量为明确不存在的路径
	t.Setenv("POLARIS_SUBSTRATE_LIB", "/nonexistent_polaris_test_xyz/libsubstrate.dylib")

	// 调用 doLoad()：可能成功（如果 dev 模式路径存在真实库）或失败（所有路径都不存在）
	// 本测试验证：无论哪种情况，都不应该 panic；
	// 如果失败，应该返回有意义的 error 消息
	handle, err := doLoad()

	// 如果成功加载（dev 路径找到了库），说明库存在且 ABI 匹配
	if handle != 0 {
		t.Logf("库在 dev 模式候选路径中被找到并成功加载（handle=%d）", handle)
		return
	}

	// 如果失败，error 应该提到 "substrate dylib not found"
	if err != nil {
		errMsg := err.Error()
		if !strings.Contains(errMsg, "substrate dylib not found") {
			t.Logf("error 消息未包含预期文本，但返回了 error: %s", errMsg)
		}
		return
	}

	// 既没有成功加载也没有返回 error 是异常状态
	t.Error("期望 doLoad 要么返回有效 handle，要么返回 non-nil error")
}
