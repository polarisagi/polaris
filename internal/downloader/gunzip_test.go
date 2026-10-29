package downloader

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestDownloadGunzip(t *testing.T) {
	origContent := []byte("#!/bin/sh\necho ffmpeg version 6.0\n")

	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	if _, err := gw.Write(origContent); err != nil {
		t.Fatalf("failed to gzip: %v", err)
	}
	gw.Close()

	client := &http.Client{
		Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewReader(gzBuf.Bytes())),
			}
		}),
	}

	pinResolvedProxy(t, "") // direct

	tmpDir := t.TempDir()
	destPath := filepath.Join(tmpDir, "ffmpeg-binary")

	ctx := context.Background()
	if err := DownloadGunzip(ctx, client, "http://dummy/ffmpeg.gz", destPath); err != nil {
		t.Fatalf("DownloadGunzip failed: %v", err)
	}

	readBack, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("failed to read destPath: %v", err)
	}

	if !bytes.Equal(readBack, origContent) {
		t.Errorf("content mismatch: got %q, want %q", string(readBack), string(origContent))
	}
}
