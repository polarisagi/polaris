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

	// 直接操作 proxyState 单例字段，绕过 probeOnce 触发探测。
	s := getProxy()
	old := s.resolved
	s.resolved = "" // direct
	defer func() { s.resolved = old }()

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
	// 绕过 probeOnce 避免触发外网镜像请求。
	s := getProxy()
	old := s.resolved
	s.resolved = ""
	defer func() { s.resolved = old }()

	// 使用短路超时：此测试只验证无效 URL 返回错误，不关心镜像降级细节。
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	err := DownloadExtractLibs(ctx, http.DefaultClient, "http://127.0.0.1:0/fake.bz2", dir)
	if err == nil {
		t.Errorf("expected error connecting")
	}
}

func TestDownloadExtractTarBz2_Gz(t *testing.T) {
	// 绕过 probeOnce 避免触发外网镜像请求。
	s := getProxy()
	old := s.resolved
	s.resolved = ""
	defer func() { s.resolved = old }()

	// 使用短路超时：此测试只验证无效 URL 返回错误，不关心镜像降级细节。
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	err := DownloadExtractTarBz2(ctx, http.DefaultClient, "http://127.0.0.1:0/fake.bz2", t.TempDir(), nil)
	if err == nil {
		t.Errorf("expected err")
	}

	// 重置超时用于第二次调用
	ctx2, cancel2 := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel2()

	err = DownloadExtractTarGz(ctx2, http.DefaultClient, "http://127.0.0.1:0/fake.gz", t.TempDir(), nil)
	if err == nil {
		t.Errorf("expected err")
	}
}
