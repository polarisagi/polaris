package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunHookScript(t *testing.T) {
	// Create a dummy script
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "hook.sh")

	// Test sanitizeHookEnv implicitly via RunHookScript
	os.Setenv("TEST_SENSITIVE_VAR", "secret")
	defer os.Unsetenv("TEST_SENSITIVE_VAR")

	script := `#!/bin/sh
echo "HOME=$HOME"
echo "TEST_SENSITIVE_VAR=$TEST_SENSITIVE_VAR"
echo "CUSTOM_ENV=$CUSTOM_ENV"
exit 0
`
	if err := os.WriteFile(scriptPath, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write script: %v", err)
	}

	ctx := context.Background()
	exitCode, out, err := RunHookScript(ctx, scriptPath, []string{"CUSTOM_ENV=yes"}, 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("expected exit code 0, got %d", exitCode)
	}
	if strings.Contains(out, "TEST_SENSITIVE_VAR=secret") {
		t.Fatalf("sensitive var leaked: %s", out)
	}
	if !strings.Contains(out, "CUSTOM_ENV=yes") {
		t.Fatalf("custom env not passed: %s", out)
	}

	// Test non-zero exit
	scriptFail := `#!/bin/sh
exit 2
`
	scriptPathFail := filepath.Join(tmpDir, "hook_fail.sh")
	os.WriteFile(scriptPathFail, []byte(scriptFail), 0755)

	exitCode2, _, err2 := RunHookScript(ctx, scriptPathFail, nil, 5*time.Second)
	if err2 != nil {
		t.Fatalf("unexpected error for non-zero exit: %v", err2)
	}
	if exitCode2 != 2 {
		t.Fatalf("expected exit code 2, got %d", exitCode2)
	}

	// Test timeout
	scriptTimeout := `#!/bin/sh
sleep 10
exit 0
`
	scriptPathTimeout := filepath.Join(tmpDir, "hook_timeout.sh")
	os.WriteFile(scriptPathTimeout, []byte(scriptTimeout), 0755)

	exitCode3, _, err3 := RunHookScript(ctx, scriptPathTimeout, nil, 100*time.Millisecond)
	if err3 != nil {
		t.Fatalf("unexpected error for timeout: %v", err3)
	}
	if exitCode3 != 1 {
		t.Fatalf("expected exit code 1 for timeout, got %d", exitCode3)
	}
}
