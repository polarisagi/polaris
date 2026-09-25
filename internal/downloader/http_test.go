package downloader

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDownloadChunk(t *testing.T) {
	clientHTTP := &http.Client{
		Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
			if req.Header.Get("Range") != "" {
				return &http.Response{
					StatusCode: http.StatusPartialContent,
					Body:       io.NopCloser(strings.NewReader("chunk")),
				}
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("full")),
			}
		}),
	}

	dir := t.TempDir()
	part := filepath.Join(dir, "test.part")

	// Full download
	err := downloadChunk(context.Background(), clientHTTP, "http://dummy", part, 0)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	content, _ := os.ReadFile(part)
	if string(content) != "full" {
		t.Errorf("expected full, got %s", string(content))
	}

	// Range download
	err = downloadChunk(context.Background(), clientHTTP, "http://dummy", part, 4)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	content, _ = os.ReadFile(part)
	// It should append "chunk" to "full"
	if string(content) != "fullchunk" {
		t.Errorf("expected fullchunk, got %s", string(content))
	}

	// Test nil client error
	err = downloadChunk(context.Background(), nil, "http://dummy", part, 0)
	if err == nil {
		t.Errorf("expected error for nil client")
	}
}

func TestDownloadFile(t *testing.T) {
	clientHTTP := &http.Client{
		Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("data")),
			}
		}),
	}

	pinResolvedProxy(t, "") // direct

	dir := t.TempDir()
	dest := filepath.Join(dir, "test.txt")

	err := DownloadFile(context.Background(), clientHTTP, "http://dummy", dest)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	content, _ := os.ReadFile(dest)
	if string(content) != "data" {
		t.Errorf("expected data, got %s", string(content))
	}

	// Idempotent
	err = DownloadFile(context.Background(), clientHTTP, "http://dummy", dest)
	if err != nil {
		t.Fatalf("expected nil err on retry, got %v", err)
	}
}

func TestDownloadExtract(t *testing.T) {
	clientHTTP := &http.Client{
		Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("dummy archive")),
			}
		}),
	}

	err := downloadExtract(context.Background(), clientHTTP, "http://dummy", func(path string) error {
		return nil // mock success
	})
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
}

func TestDownloadExtractLibs(t *testing.T) {
	dir := t.TempDir()
	pinResolvedProxy(t, "")

	// 候选列表含公网镜像，断网 client 让每个候选即刻失败，测试不出网。
	offlineClient := &http.Client{Transport: erroringRoundTripper{}}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	err := DownloadExtractLibs(ctx, offlineClient, "http://127.0.0.1:0/fake.bz2", dir)
	if err == nil {
		t.Errorf("expected error connecting")
	}
}

func TestDownloadExtractTarBz2_Gz(t *testing.T) {
	pinResolvedProxy(t, "")

	// 候选列表含公网镜像，断网 client 让每个候选即刻失败，测试不出网。
	offlineClient := &http.Client{Transport: erroringRoundTripper{}}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	err := DownloadExtractTarBz2(ctx, offlineClient, "http://127.0.0.1:0/fake.bz2", t.TempDir(), nil)
	if err == nil {
		t.Errorf("expected err")
	}
}
