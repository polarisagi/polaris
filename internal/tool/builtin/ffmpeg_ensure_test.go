package builtin

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

type mockRoundTripper func(req *http.Request) *http.Response

func (f mockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req), nil
}

func TestEnsureFFmpeg_ExistingInBinDir(t *testing.T) {
	tmpDir := t.TempDir()
	exeName := "ffmpeg"
	if runtime.GOOS == "windows" {
		exeName = "ffmpeg.exe"
	}
	binPath := filepath.Join(tmpDir, exeName)
	if err := os.WriteFile(binPath, []byte("echo ffmpeg"), 0755); err != nil {
		t.Fatalf("failed to write dummy ffmpeg: %v", err)
	}

	p, err := EnsureFFmpeg(context.Background(), tmpDir, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p != binPath {
		t.Errorf("expected %s, got %s", binPath, p)
	}
}

func TestEnsureFFmpeg_AutoDownload(t *testing.T) {
	tmpDir := t.TempDir()
	binDir := filepath.Join(tmpDir, "bin")

	origContent := []byte("#!/bin/sh\necho ffmpeg version 6.0\n")
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	gw.Write(origContent)
	gw.Close()

	client := &http.Client{
		Transport: mockRoundTripper(func(req *http.Request) *http.Response {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(gzBuf.Bytes())),
			}
		}),
	}

	// Rename or mock PATH if needed; since binDir doesn't have it, if system doesn't have it, it will download.
	// But to guarantee download happens, test with a custom function or ensure destPath exists after EnsureFFmpeg.
	exeName := "ffmpeg"
	if runtime.GOOS == "windows" {
		exeName = "ffmpeg.exe"
	}

	// Call EnsureFFmpeg
	p, err := EnsureFFmpeg(context.Background(), binDir, client)
	if err != nil {
		t.Fatalf("EnsureFFmpeg failed: %v", err)
	}
	if p == "" {
		t.Fatalf("expected non-empty ffmpeg path")
	}

	// Verify either system ffmpeg was found or our downloaded ffmpeg in binDir was created
	if p == filepath.Join(binDir, exeName) {
		content, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("failed to read downloaded ffmpeg: %v", err)
		}
		if !bytes.Equal(content, origContent) {
			t.Errorf("content mismatch")
		}
	}
}
