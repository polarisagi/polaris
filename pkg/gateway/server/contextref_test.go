package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── isSensitivePath ───────────────────────────────────────────────────────────

func TestIsSensitivePath_BlocksKnownPaths(t *testing.T) {
	blocked := []string{
		".ssh/id_rsa",
		".aws/credentials",
		"/home/user/.kube/config",
		".env",
		".docker/config.json",
		".npmrc",
		"id_rsa",
	}
	for _, p := range blocked {
		if !isSensitivePath(p) {
			t.Errorf("should block: %s", p)
		}
	}
}

func TestIsSensitivePath_AllowsSafePaths(t *testing.T) {
	safe := []string{
		"main.go",
		"src/main.go",
		"README.md",
		"configs/defaults.toml",
		"internal/protocol/types.go",
		"ssh",
	}
	for _, p := range safe {
		if isSensitivePath(p) {
			t.Errorf("should allow: %s", p)
		}
	}
}

// ─── resolveFile ───────────────────────────────────────────────────────────────

func TestResolveFile_FullContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	os.WriteFile(path, []byte("hello\nworld\n"), 0644)

	e := NewContextRefExpander(nil, WithWorkDir(dir))
	content, bytes, err := e.resolveFile(context.Background(), "test.txt")
	if err != nil {
		t.Fatalf("resolveFile: %v", err)
	}
	if bytes != 12 {
		t.Errorf("bytes: want 12, got %d", bytes)
	}
	if !strings.Contains(content, "hello") {
		t.Errorf("content should contain hello, got %q", content)
	}
}

func TestResolveFile_WithLineRange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "multi.txt")
	lines := []string{"line1", "line2", "line3", "line4", "line5"}
	os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0644)

	e := NewContextRefExpander(nil, WithWorkDir(dir))
	content, _, err := e.resolveFile(context.Background(), "multi.txt:2-4")
	if err != nil {
		t.Fatalf("resolveFile: %v", err)
	}
	want := "line2\nline3\nline4"
	if content != want {
		t.Errorf("line range: want %q, got %q", want, content)
	}
}

func TestResolveFile_SingleLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "single.txt")
	os.WriteFile(path, []byte("a\nb\nc\n"), 0644)

	e := NewContextRefExpander(nil, WithWorkDir(dir))
	content, _, err := e.resolveFile(context.Background(), "single.txt:3")
	if err != nil {
		t.Fatalf("resolveFile: %v", err)
	}
	if content != "c" {
		t.Errorf("single line: want %q, got %q", "c", content)
	}
}

func TestResolveFile_SensitivePath_Blocked(t *testing.T) {
	e := NewContextRefExpander(nil)
	_, _, err := e.resolveFile(context.Background(), ".ssh/id_rsa")
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Errorf("should block sensitive path, got: %v", err)
	}
}

func TestResolveFile_NotFound(t *testing.T) {
	e := NewContextRefExpander(nil)
	_, _, err := e.resolveFile(context.Background(), "nonexistent.go")
	if err == nil {
		t.Error("should error on nonexistent file")
	}
}

// ─── resolveURL ────────────────────────────────────────────────────────────────

func TestResolveURL_Success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("url content"))
	}))
	defer ts.Close()

	e := NewContextRefExpander(ts.Client())
	content, bytes, err := e.resolveURL(context.Background(), ts.URL)
	if err != nil {
		t.Fatalf("resolveURL: %v", err)
	}
	if bytes != 11 {
		t.Errorf("bytes: want 11, got %d", bytes)
	}
	if content != "url content" {
		t.Errorf("content: want %q, got %q", "url content", content)
	}
}

func TestResolveURL_HTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	e := NewContextRefExpander(ts.Client())
	_, _, err := e.resolveURL(context.Background(), ts.URL)
	if err == nil {
		t.Error("should error on HTTP 404")
	}
}

func TestResolveURL_Timeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer ts.Close()

	e := NewContextRefExpander(ts.Client())
	_, _, err := e.resolveURL(context.Background(), ts.URL)
	if err == nil {
		t.Error("should error on timeout")
	}
}

// ─── Expand ────────────────────────────────────────────────────────────────────

func TestExpand_NoRefs(t *testing.T) {
	e := NewContextRefExpander(nil)
	out, report := e.Expand(context.Background(), "hello world")
	if out != "hello world" {
		t.Errorf("no refs: should passthrough, got %q", out)
	}
	if report.OverBudget {
		t.Error("no refs should not be over budget")
	}
}

func TestExpand_FileRef(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hi.txt")
	os.WriteFile(path, []byte("file content"), 0644)

	e := NewContextRefExpander(nil, WithWorkDir(dir))
	input := `read this: @file:"hi.txt"`
	out, report := e.Expand(context.Background(), input)

	if !strings.Contains(out, "file content") {
		t.Errorf("expanded content missing: %q", out)
	}
	if len(report.Resolved) != 1 {
		t.Errorf("should have 1 resolved ref, got %d", len(report.Resolved))
	}
}

func TestExpand_FileAndURL(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("local notes"), 0644)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("web content"))
	}))
	defer ts.Close()

	e := NewContextRefExpander(ts.Client(), WithWorkDir(dir))
	input := `read @file:"notes.txt" and @url:"` + ts.URL + `"`
	out, report := e.Expand(context.Background(), input)

	if !strings.Contains(out, "local notes") {
		t.Errorf("file content missing: %q", out)
	}
	if !strings.Contains(out, "web content") {
		t.Errorf("url content missing: %q", out)
	}
	if len(report.Resolved) != 2 {
		t.Errorf("should have 2 resolved refs, got %d", len(report.Resolved))
	}
}

func TestExpand_SensitiveRefBlocked(t *testing.T) {
	e := NewContextRefExpander(nil)
	input := `read @file:".ssh/id_rsa"`
	out, report := e.Expand(context.Background(), input)

	if strings.Contains(out, "BEGIN") || strings.Contains(out, "PRIVATE") {
		t.Error("should not expand sensitive file")
	}
	if len(report.Skipped) != 1 {
		t.Errorf("should have 1 skipped ref, got %d", len(report.Skipped))
	}
}

func TestExpand_GitRefUnsupported(t *testing.T) {
	e := NewContextRefExpander(nil)
	input := `check @git:"main"`
	out, report := e.Expand(context.Background(), input)

	_ = out
	if len(report.Skipped) > 0 {
		t.Logf("git ref: %s", report.Skipped[0])
	}
}

func TestExpand_MultipleSameRef(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "doc.txt"), []byte("same content"), 0644)

	e := NewContextRefExpander(nil, WithWorkDir(dir))
	input := `@file:"doc.txt" and again @file:"doc.txt"`
	out, report := e.Expand(context.Background(), input)

	if strings.Count(out, "same content") != 2 {
		t.Errorf("should expand both occurrences, got %q", out)
	}
	if len(report.Resolved) != 1 {
		t.Errorf("should have 1 unique resolved entry, got %d", len(report.Resolved))
	}
}
