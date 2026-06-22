package server

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
