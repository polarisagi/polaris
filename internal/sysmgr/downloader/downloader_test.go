package downloader

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── urlBaseName ────────────────────────────────────────────────────────────

func TestURLBaseName(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"https://example.com/file.tar.gz", "file.tar.gz"},
		{"https://example.com/path/to/archive.zip?v=1&t=2", "archive.zip"},
		{"https://example.com/release#v1.0", "release"},
		{"https://example.com/a/b/c.dylib", "c.dylib"},
		{"file.txt", "file.txt"},
	}
	for _, tc := range cases {
		if got := urlBaseName(tc.url); got != tc.want {
			t.Errorf("urlBaseName(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

// ── lastSegment ────────────────────────────────────────────────────────────

func TestLastSegment(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"/a/b/c.txt", "c.txt"},
		{"a/b/c.txt", "c.txt"},
		{"c.txt", "c.txt"},
		{"a\\b\\c.so", "c.so"},
		{"/", ""},
	}
	for _, tc := range cases {
		if got := lastSegment(tc.path); got != tc.want {
			t.Errorf("lastSegment(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// ── joinPath ───────────────────────────────────────────────────────────────

func TestJoinPath(t *testing.T) {
	if got := joinPath("/a/b", "c.so"); got != "/a/b/c.so" {
		t.Errorf("got %q", got)
	}
}

// ── Get ────────────────────────────────────────────────────────────────────

func TestGet_NilClient(t *testing.T) {
	_, err := Get(context.Background(), nil, "https://example.com")
	if err == nil {
		t.Error("expected error for nil client, got nil")
	}
}

func TestGet_200OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("body"))
	}))
	defer srv.Close()

	resp, err := Get(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if string(data) != "body" {
		t.Errorf("body: got %q, want %q", data, "body")
	}
}

func TestGet_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := Get(context.Background(), srv.Client(), srv.URL)
	if err == nil {
		t.Error("expected error for 404, got nil")
	}
}

// ── ExtractTarBz2 ──────────────────────────────────────────────────────────

func TestExtractTarGz(t *testing.T) {
	dir := t.TempDir()
	// 构建 tar.gz 内存流
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	content := "hello from tar.gz"
	_ = tw.WriteHeader(&tar.Header{
		Name:     "sub/file.txt",
		Mode:     0o644,
		Size:     int64(len(content)),
		Typeflag: tar.TypeReg,
	})
	_, _ = io.WriteString(tw, content)
	_ = tw.Close()
	_ = gzw.Close()

	destFile := filepath.Join(dir, "file.txt")
	err := ExtractTarGz(&buf, dir, func(name string) (string, bool) {
		if strings.HasSuffix(name, "file.txt") {
			return destFile, true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("ExtractTarGz: %v", err)
	}
	data, err := os.ReadFile(destFile)
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(data) != content {
		t.Errorf("content: got %q, want %q", data, content)
	}
}

func TestExtractTarGz_NoTargetFiles(t *testing.T) {
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	_ = tw.WriteHeader(&tar.Header{
		Name:     "other.bin",
		Mode:     0o644,
		Size:     3,
		Typeflag: tar.TypeReg,
	})
	_, _ = io.WriteString(tw, "bin")
	_ = tw.Close()
	_ = gzw.Close()

	// mapper 全拒绝 → 应返回"no target files"错误
	err := ExtractTarGz(&buf, t.TempDir(), func(_ string) (string, bool) { return "", false })
	if err == nil {
		t.Error("expected error when no files extracted, got nil")
	}
}

// ── ExtractZip ─────────────────────────────────────────────────────────────

func TestExtractZip(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "test.zip")

	// 构建 zip 文件
	zf, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	content := "hello from zip"
	fw, _ := zw.Create("data/hello.txt")
	_, _ = io.WriteString(fw, content)
	_ = zw.Close()
	_ = zf.Close()

	destFile := filepath.Join(dir, "hello_out.txt")
	err = ExtractZip(zipPath, dir, func(name string) (string, bool) {
		if strings.HasSuffix(name, "hello.txt") {
			return destFile, true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("ExtractZip: %v", err)
	}
	data, err := os.ReadFile(destFile)
	if err != nil {
		t.Fatalf("read extracted: %v", err)
	}
	if string(data) != content {
		t.Errorf("content: got %q, want %q", data, content)
	}
}

func TestExtractZip_PathTraversal(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "traversal.zip")

	zf, err := os.Create(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(zf)
	// 尝试路径穿越
	fw, _ := zw.Create("../../etc/passwd")
	_, _ = io.WriteString(fw, "should not appear")
	_ = zw.Close()
	_ = zf.Close()

	// 传 nil mapper → 走路径穿越防护逻辑
	err = ExtractZip(zipPath, dir, nil)
	// 应返回 "no files extracted"（穿越路径被跳过）
	if err == nil {
		t.Error("expected error due to traversal skip yielding no files, got nil")
	}
}

func TestExtractZip_NoMatchingFiles(t *testing.T) {
	dir := t.TempDir()
	zipPath := filepath.Join(dir, "empty.zip")

	zf, _ := os.Create(zipPath)
	zw := zip.NewWriter(zf)
	fw, _ := zw.Create("other.bin")
	_, _ = io.WriteString(fw, "x")
	_ = zw.Close()
	_ = zf.Close()

	err := ExtractZip(zipPath, dir, func(_ string) (string, bool) { return "", false })
	if err == nil {
		t.Error("expected error when no files match mapper, got nil")
	}
}

// ── writeFromReader ────────────────────────────────────────────────────────

func TestWriteFromReader(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "sub", "out.txt")
	content := "atomic write test"
	err := writeFromReader(strings.NewReader(content), dest, 0o644)
	if err != nil {
		t.Fatalf("writeFromReader: %v", err)
	}
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(data) != content {
		t.Errorf("content: got %q, want %q", data, content)
	}
}
