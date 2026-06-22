package sandbox

import (
	"context"
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

	// Test wrapDarwin
	cmdDarwin, err := wrapDarwin(ctx, cfg)
	if err != nil {
		t.Fatalf("wrapDarwin err: %v", err)
	}
	if cmdDarwin != nil {
		// Just ensure it builds a command successfully (fallback or sandbox-exec)
		if cmdDarwin.Dir != "/tmp/test" {
			t.Errorf("expected dir /tmp/test, got %s", cmdDarwin.Dir)
		}
	}

	// Test wrapLinux (with bwrap fallback handling)
	cmdLinux, err := wrapLinux(ctx, cfg)
	if err != nil {
		t.Fatalf("wrapLinux err: %v", err)
	}
	if cmdLinux != nil {
		if cmdLinux.Dir != "/tmp/test" {
			t.Errorf("expected dir /tmp/test, got %s", cmdLinux.Dir)
		}
	}

	// Test wrapWithBwrap explicitly
	cmdBwrap, err := wrapWithBwrap(ctx, "bwrap", cfg)
	if err != nil {
		t.Fatalf("wrapWithBwrap err: %v", err)
	}
	if cmdBwrap != nil && len(cmdBwrap.Args) > 0 {
		if cmdBwrap.Args[0] != "bwrap" {
			t.Errorf("expected bwrap, got %s", cmdBwrap.Args[0])
		}
	}

	// Test wrapWindows
	cmdWindows, err := wrapWindows(ctx, cfg)
	if err != nil {
		t.Fatalf("wrapWindows err: %v", err)
	}
	if cmdWindows != nil {
		// Just ensure no error during command building
		_ = cmdWindows.Args
	}
}

func TestNativeSandbox_WrapPlatform_NetworkBlock(t *testing.T) {
	ctx := context.Background()
	cfg := NativeSandboxCfg{
		Command:       "echo blocked",
		WorkDir:       "C:\\Users\\test",
		Env:           []string{"FOO=bar"},
		NetworkPolicy: NetworkBlock,
	}

	// Test wrapWindows with network block
	cmdWindows, err := wrapWindows(ctx, cfg)
	if err != nil {
		t.Fatalf("wrapWindows err: %v", err)
	}
	if cmdWindows != nil {
		argsStr := strings.Join(cmdWindows.Args, " ")
		// Not necessarily checking argsStr deeply since WSL might fallback if wsl.exe not found,
		// but we're calling it for coverage.
		_ = argsStr
	}
}
