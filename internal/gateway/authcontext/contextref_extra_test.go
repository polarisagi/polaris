package authcontext

import (
	"testing"
)

func TestContextRefWithWorkDir(t *testing.T) {
	c := &ContextRefExpander{}
	WithWorkDir("/tmp")(c)
	if c.workDir != "/tmp" {
		t.Errorf("expected /tmp")
	}
}

func TestContextRefExpander_ResolveFile_Security(t *testing.T) {
	expander := &ContextRefExpander{}

	// 1. workDir empty
	_, _, err := expander.resolveFile(context.Background(), "test.txt")
	if err == nil || !strings.Contains(err.Error(), "workDir 未配置") {
		t.Errorf("expected fail-closed error for empty workDir, got: %v", err)
	}

	workDir := t.TempDir()
	expander.workDir = workDir

	// Create a dummy file outside workDir
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	os.WriteFile(outsideFile, []byte("secret"), 0644)

	// Create a file in workDir that is a symlink to outsideFile
	symlinkPath := filepath.Join(workDir, "link.txt")
	os.Symlink(outsideFile, symlinkPath)

	// 2. Traversal attempt
	_, _, err = expander.resolveFile(context.Background(), "../secret.txt")
	if err == nil || !strings.Contains(err.Error(), "路径穿越") {
		t.Errorf("expected path traversal error, got: %v", err)
	}

	// 3. Absolute path outside workDir
	_, _, err = expander.resolveFile(context.Background(), outsideFile)
	if err == nil || !strings.Contains(err.Error(), "路径穿越") {
		t.Errorf("expected path traversal error for absolute path outside workdir, got: %v", err)
	}

	// 4. Symlink bypass
	_, _, err = expander.resolveFile(context.Background(), "link.txt")
	if err == nil || !strings.Contains(err.Error(), "路径穿越") {
		t.Errorf("expected path traversal error for symlink, got: %v", err)
	}

	// 5. Sensitive path
	sensitivePath := "/etc/passwd"
	_, _, err = expander.resolveFile(context.Background(), sensitivePath)
	if err == nil || !strings.Contains(err.Error(), "路径穿越") { // Actually, this will hit path traversal first if not in /etc workdir
		// If we set workDir to /etc, it will hit sensitive path
	}
	expander.workDir = "/etc"
	_, _, err = expander.resolveFile(context.Background(), "/etc/passwd")
	if err == nil || !strings.Contains(err.Error(), "sensitive path") {
		t.Errorf("expected sensitive path error, got: %v", err)
	}
}
