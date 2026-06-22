package mcp

import (
	"os"
	"strings"
	"testing"
)

func TestSanitizeParentEnv(t *testing.T) {
	// Set a mix of allowed and unallowed env variables
	os.Setenv("PATH", "/usr/bin")
	os.Setenv("HOME", "/home/user")
	os.Setenv("SECRET_KEY", "super_secret")
	os.Setenv("MY_APP_TOKEN", "token123")

	env := sanitizeParentEnv()

	hasPath := false
	hasHome := false
	hasSecret := false
	hasToken := false

	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			hasPath = true
		}
		if strings.HasPrefix(e, "HOME=") {
			hasHome = true
		}
		if strings.HasPrefix(e, "SECRET_KEY=") {
			hasSecret = true
		}
		if strings.HasPrefix(e, "MY_APP_TOKEN=") {
			hasToken = true
		}
	}

	if !hasPath {
		t.Errorf("expected PATH to be in sanitized env")
	}
	if !hasHome {
		t.Errorf("expected HOME to be in sanitized env")
	}
	if hasSecret {
		t.Errorf("expected SECRET_KEY to be filtered out")
	}
	if hasToken {
		t.Errorf("expected MY_APP_TOKEN to be filtered out")
	}

	os.Unsetenv("PATH")
	os.Unsetenv("HOME")
	os.Unsetenv("SECRET_KEY")
	os.Unsetenv("MY_APP_TOKEN")
}
