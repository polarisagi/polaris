package sandbox

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestNativeSandbox_WrapPlatform(t *testing.T) {
	ctx := context.Background()
	cfg := NativeSandboxCfg{
		Command:       "echo hello",
		WorkDir:       "/tmp/test",
		Env:           []string{"FOO=bar"},
		NetworkPolicy: NetworkAllow,
	}

	// Test wrapDarwin — NetworkPolicy=NetworkAllow，即使 sandbox-exec 缺失也应
	// 优雅降级为 bare，不会 fail-closed（只在 NetworkBlock 时才拒绝）。
	cmdDarwin, _, err := wrapDarwin(ctx, cfg)
	if err != nil {
		t.Fatalf("wrapDarwin err: %v", err)
	}
	if cmdDarwin != nil {
		// Just ensure it builds a command successfully (fallback or sandbox-exec)
		if cmdDarwin.Dir != "/tmp/test" {
			t.Errorf("expected dir /tmp/test, got %s", cmdDarwin.Dir)
		}
	}

	// Test wrapLinux (with bwrap fallback handling) — 同样 NetworkAllow，不应 fail-closed。
	cmdLinux, _, err := wrapLinux(ctx, cfg)
	if err != nil {
		t.Fatalf("wrapLinux err: %v", err)
	}
	if cmdLinux != nil {
		if cmdLinux.Dir != "/tmp/test" {
			t.Errorf("expected dir /tmp/test, got %s", cmdLinux.Dir)
		}
	}

	// Test wrapWithBwrap explicitly（签名未变，NetworkPolicy 不影响这个底层构造函数）
	cmdBwrap, err := wrapWithBwrap(ctx, "bwrap", cfg)
	if err != nil {
		t.Fatalf("wrapWithBwrap err: %v", err)
	}
	if cmdBwrap != nil && len(cmdBwrap.Args) > 0 {
		if cmdBwrap.Args[0] != "bwrap" {
			t.Errorf("expected bwrap, got %s", cmdBwrap.Args[0])
		}
	}

	// Test wrapWindows — NetworkAllow，wsl.exe 缺失时优雅降级为 bare。
	cmdWindows, _, err := wrapWindows(ctx, cfg)
	if err != nil {
		t.Fatalf("wrapWindows err: %v", err)
	}
	if cmdWindows != nil {
		// Just ensure no error during command building
		_ = cmdWindows.Args
	}
}

// TestNativeSandbox_WrapPlatform_NetworkBlock 验证 NetworkPolicy=NetworkBlock 时
// wrapWindows 的两种正确结果之一：
//  1. wsl.exe 存在 → 正常返回带 unshare --net 的 argv（method=wsl2）
//  2. wsl.exe 不存在 → fail-closed 返回 error（HE-Rule 2，不静默裸跑）
//
// 两种结果都不应该是"静默返回一个不带任何隔离声明的裸命令"。
func TestNativeSandbox_WrapPlatform_NetworkBlock(t *testing.T) {
	ctx := context.Background()
	cfg := NativeSandboxCfg{
		Command:       "echo blocked",
		WorkDir:       "C:\\Users\\test",
		Env:           []string{"FOO=bar"},
		NetworkPolicy: NetworkBlock,
	}

	cmdWindows, method, err := wrapWindows(ctx, cfg)
	if _, lookErr := exec.LookPath("wsl.exe"); lookErr != nil {
		// 本机（非 Windows/无 WSL2）必然找不到 wsl.exe：期望 fail-closed。
		if err == nil {
			t.Fatalf("expected fail-closed error when wsl.exe missing and NetworkBlock requested, got method=%q", method)
		}
		return
	}
	// 极少数场景下本机真的装了 wsl.exe：应正常返回且带 unshare --net。
	if err != nil {
		t.Fatalf("wrapWindows err: %v", err)
	}
	if cmdWindows != nil {
		argsStr := strings.Join(cmdWindows.Args, " ")
		if !strings.Contains(argsStr, "unshare") {
			t.Errorf("expected unshare --net in argv when NetworkBlock requested, got: %s", argsStr)
		}
	}
}
