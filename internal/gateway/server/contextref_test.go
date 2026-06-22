package server

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsSensitivePath(t *testing.T) {
	cases := []struct {
		path  string
		block bool
	}{
		{".ssh/id_rsa", true},
		{"/root/.aws/credentials", true},
		{"id_rsa", true},
		{"safe_file.txt", false},
		{".env", true},
		{"/var/www/html/index.php", false},
		{"/etc/.git/config", true},
	}
	for _, c := range cases {
		if isSensitivePath(c.path) != c.block {
			t.Errorf("isSensitivePath(%q) = %v, expected %v", c.path, !c.block, c.block)
		}
	}
}

func TestContextRefExpander_Expand(t *testing.T) {
	exp := NewContextRefExpander(http.DefaultClient, WithMaxExpandTokens(100))

	// Simple skip case
	text := `@file:".ssh/id_rsa"`
	_, report := exp.Expand(context.Background(), text)
	if report.OverBudget {
		t.Errorf("expected not over budget")
	}
	if len(report.Skipped) != 1 {
		t.Errorf("expected skipped")
	}

	// File resolution
	tmpDir := t.TempDir()
	exp.workDir = tmpDir
	testFile := filepath.Join(tmpDir, "test.txt")
	os.WriteFile(testFile, []byte("line1\nline2\nline3\n"), 0644)

	text2 := `@file:"test.txt:2-3"`
	res2, _ := exp.Expand(context.Background(), text2)
	if !strings.Contains(res2, "line2\nline3") {
		t.Errorf("expected line2 and line3, got %s", res2)
	}

	text3 := `@file:"test.txt:1"`
	res3, _ := exp.Expand(context.Background(), text3)
	if !strings.Contains(res3, "line1") {
		t.Errorf("expected line1, got %s", res3)
	}
}

func TestContextRefExpander_URL(t *testing.T) {
	exp := NewContextRefExpander(nil) // Nil client should fail explicitly

	text := `@url:"http://example.com"`
	_, report := exp.Expand(context.Background(), text)
	if len(report.Skipped) != 1 {
		t.Errorf("expected skip due to nil client")
	}
}
