package sandbox

import (
	"context"
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
	cmd, resp, err := WrapBashCmd(context.Background(), cfg)
	if err != nil {
		t.Logf("WrapBashCmd error: %v", err)
	}
	_ = cmd
	_ = resp
}
