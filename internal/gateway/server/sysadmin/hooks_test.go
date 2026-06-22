package sysadmin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHookRunner(t *testing.T) {
	dir := t.TempDir()
	hr := &HookRunner{dir: dir}

	// Not found
	blocked, _ := hr.FireBefore("no_hook", nil)
	if blocked {
		t.Errorf("expected false for missing hook")
	}

	// Create passing script
	hook1 := filepath.Join(dir, "pass.sh")
	os.WriteFile(hook1, []byte(`#!/bin/sh
exit 0`), 0755)

	blocked, reason := hr.FireBefore("pass.sh", nil)
	if blocked {
		t.Errorf("expected false for passing hook, got reason: %s", reason)
	}

	// Create failing script
	hook2 := filepath.Join(dir, "fail.sh")
	os.WriteFile(hook2, []byte(`#!/bin/sh
echo blocked
exit 1`), 0755)

	blocked, reason = hr.FireBefore("fail.sh", nil)
	if !blocked {
		t.Errorf("expected true for failing hook")
	}
	if strings.TrimSpace(reason) != "blocked" {
		t.Errorf("expected blocked reason, got %s", reason)
	}

	// Fire async
	hr.Fire("pass.sh", nil)
	time.Sleep(100 * time.Millisecond) // wait for async hook to finish to avoid t.TempDir race
}
