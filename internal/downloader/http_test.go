package downloader

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDownloadChunk(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			w.WriteHeader(http.StatusPartialContent)
			w.Write([]byte("chunk"))
		} else {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("full"))
		}
	}))
	defer ts.Close()

	dir := t.TempDir()
	part := filepath.Join(dir, "test.part")

	// Full download
	err := downloadChunk(context.Background(), ts.Client(), ts.URL, part, 0)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	content, _ := os.ReadFile(part)
	if string(content) != "full" {
		t.Errorf("expected full, got %s", string(content))
	}

	// Range download
	err = downloadChunk(context.Background(), ts.Client(), ts.URL, part, 4)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	content, _ = os.ReadFile(part)
	// It should append "chunk" to "full"
	if string(content) != "fullchunk" {
		t.Errorf("expected fullchunk, got %s", string(content))
	}

	// Test nil client error
	err = downloadChunk(context.Background(), nil, ts.URL, part, 0)
	if err == nil {
		t.Errorf("expected error for nil client")
	}
}

func TestDownloadFile(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data"))
	}))
	defer ts.Close()

	// 直接操作 proxyState 单例字段，绕过 probeOnce 触发探测。
	s := getProxy()
	old := s.resolved
	s.resolved = "" // direct
	defer func() { s.resolved = old }()

	dir := t.TempDir()
	dest := filepath.Join(dir, "test.txt")

	err := DownloadFile(context.Background(), ts.Client(), ts.URL, dest)
	if err != nil {
		t.Fatalf("expected nil err, got %v", err)
	}
	content, _ := os.ReadFile(dest)
	if string(content) != "data" {
		t.Errorf("expected data, got %s", string(content))
	}

	// Idempotent
	err = DownloadFile(context.Background(), ts.Client(), ts.URL, dest)
	if err != nil {
		t.Fatalf("expected nil err on retry, got %v", err)
	}
}

func TestDownloadExtract(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("dummy archive"))
	}))
	defer ts.Close()

	err := downloadExtract(context.Background(), ts.Client(), ts.URL, func(path string) error {
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
