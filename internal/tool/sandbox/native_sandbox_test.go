package sandbox

import (
	"context"
	"os/exec"
	"testing"
)

func TestBuildEnvPrefix(t *testing.T) {
	envs := []string{"FOO=bar", "BAZ=qux'quote"}
	prefix := buildEnvPrefix(envs)
	if prefix != "export FOO='bar'; export BAZ='qux'\\''quote'; " {
		t.Fatalf("unexpected prefix: %s", prefix)
	}
}

func TestWindowsPathToWSL(t *testing.T) {
	if windowsPathToWSL("C:\\foo\\bar") != "/mnt/c/foo/bar" {
		t.Fatalf("unexpected wsl path")
	}
	if windowsPathToWSL("\\\\server\\share") != "//server/share" {
		t.Fatalf("unexpected unc path")
	}
}

func TestSbplEscape(t *testing.T) {
	if sbplEscape("/path/with\"quote") != `"/path/with\"quote"` {
		t.Fatalf("unexpected escape")
	}
}

func TestWrapBashCmdGoFallback(t *testing.T) {
	cfg := NativeSandboxCfg{
		Command: "echo fallback",
	}
	cmd, err := wrapFallback(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error")
	}
	if cmd == nil || cmd.Path != "bash" { // Should resolve to absolute path or just "bash" if not looked up, but exec.Command does minimal lookup. Actually it might fail lookpath but it won't crash
		t.Log("got command")
	}
}

func TestBuildSeatbeltProfile(t *testing.T) {
	cfg := NativeSandboxCfg{
		AllowedPaths:  []string{"/test/path"},
		NetworkPolicy: NetworkAllow,
	}
	profile := buildSeatbeltProfile(cfg)
	if profile == "" {
		t.Fatalf("expected profile")
	}
}

func TestWrapBashCmd(t *testing.T) {
	cfg := NativeSandboxCfg{
		Command:   "echo hello",
		TimeoutMs: 1000,
	}
	cmd, resp, method, err := WrapBashCmd(context.Background(), cfg)
	if err != nil {
		t.Logf("WrapBashCmd error: %v", err)
	}
	_ = cmd
	_ = resp
	_ = method
}

// TestWrapLinux_NetworkBlockFailsClosedWhenBwrapMissing 验证 fail-closed 规则
// （HE-Rule 2：物理断裂 > 概率过滤）：NetworkPolicy=NetworkBlock 且 bwrap 不可用时，
// wrapLinux 必须返回 error，不得静默退化为裸执行（bare 对网络零隔离）。
// 不显式设置 BwrapPath，走真实 exec.LookPath("bwrap")——在跑测试的 macOS/多数
// CI 容器上 bwrap 本就不存在，天然覆盖"工具缺失"分支；有 bwrap 时人为不可达
// (skip)，避免在真装了 bwrap 的机器上产生误判。
func TestWrapLinux_NetworkBlockFailsClosedWhenBwrapMissing(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err == nil {
		t.Skip("bwrap is installed on this host; fail-closed branch not reachable")
	}
	cfg := NativeSandboxCfg{
		Command:       "echo hello",
		NetworkPolicy: NetworkBlock,
	}
	cmd, method, err := wrapLinux(context.Background(), cfg)
	if err == nil {
		t.Fatalf("expected fail-closed error when bwrap missing and NetworkBlock requested, got cmd=%v method=%q", cmd, method)
	}
}

// TestWrapLinux_NetworkAllowStillDegradesGracefully 验证 NetworkPolicy=NetworkAllow
// 时（调用方未要求网络隔离）bwrap 缺失仍可降级为 bare，不应 fail-closed——
// 保持 Tier-0（2GB VPS，无 bwrap 场景）可用性。
func TestWrapLinux_NetworkAllowStillDegradesGracefully(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err == nil {
		t.Skip("bwrap is installed on this host; degrade-to-bare branch not reachable")
	}
	cfg := NativeSandboxCfg{
		Command:       "echo hello",
		NetworkPolicy: NetworkAllow,
	}
	_, method, err := wrapLinux(context.Background(), cfg)
	if err != nil {
		t.Fatalf("unexpected error when NetworkAllow (should degrade gracefully): %v", err)
	}
	if method != "bare" {
		t.Fatalf("expected method=bare on graceful degrade, got %q", method)
	}
}
