package downloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func shaHex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// uniqueURL 返回带唯一归档名的假 URL：downloadExtract 把归档缓存在系统临时目录的固定
// 文件名下，测试之间必须互不共用，否则会读到上一个用例留下的缓存。
func uniqueURL(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("http://dummy/verified-%d.bz2", time.Now().UnixNano())
}

func bodyClient(body string, calls *atomic.Int32) *http.Client {
	return &http.Client{Transport: mockRoundTripperFunc(func(req *http.Request) *http.Response {
		if req.Method == http.MethodGet {
			calls.Add(1)
		}
		return &http.Response{
			StatusCode:    http.StatusOK,
			ContentLength: int64(len(body)),
			Body:          io.NopCloser(strings.NewReader(body)),
			Header:        http.Header{},
		}
	})}
}

func TestDownloadExtractOpts_SHA256Match(t *testing.T) {
	pinResolvedProxy(t, "")
	var calls atomic.Int32
	extracted := ""
	err := downloadExtractOpts(context.Background(), bodyClient("payload", &calls), uniqueURL(t),
		Options{SHA256: shaHex("payload")}, func(p string) error {
			b, rerr := os.ReadFile(p)
			extracted = string(b)
			return rerr
		})
	if err != nil {
		t.Fatalf("sha 匹配应成功: %v", err)
	}
	if extracted != "payload" {
		t.Errorf("extract 回调没拿到归档内容: %q", extracted)
	}
}

// 校验失败必须阻止解压并删除归档（不能把被截断/篡改的归档交给 extract）。
func TestDownloadExtractOpts_SHA256MismatchBlocksExtract(t *testing.T) {
	pinResolvedProxy(t, "")
	var calls atomic.Int32
	u := uniqueURL(t)
	extractCalled := false
	err := downloadExtractOpts(context.Background(), bodyClient("tampered", &calls), u,
		Options{SHA256: shaHex("payload")}, func(string) error {
			extractCalled = true
			return nil
		})
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("应报 sha256 校验失败，got %v", err)
	}
	if extractCalled {
		t.Error("校验失败后不得调用 extract")
	}
	if _, statErr := os.Stat(filepath.Join(os.TempDir(), "polaris-dl-"+urlBaseName(u))); statErr == nil {
		t.Error("校验失败的归档必须被删除")
	}
	if calls.Load() != 1 {
		t.Errorf("刚下载就校验失败不应自动重下，GET 次数 %d", calls.Load())
	}
}

// 缓存命中但内容已损坏：删除后自动重下一次并通过。
func TestDownloadExtractOpts_StaleCacheRedownloaded(t *testing.T) {
	pinResolvedProxy(t, "")
	var calls atomic.Int32
	u := uniqueURL(t)
	cache := filepath.Join(os.TempDir(), "polaris-dl-"+urlBaseName(u))
	if err := os.WriteFile(cache, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(cache) })

	err := downloadExtractOpts(context.Background(), bodyClient("payload", &calls), u,
		Options{SHA256: shaHex("payload")}, func(string) error { return nil })
	if err != nil {
		t.Fatalf("陈旧缓存应被替换后成功: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("应重下一次，GET 次数 %d", calls.Load())
	}
}

func TestDownloadExtractOpts_ProgressReachesTotal(t *testing.T) {
	pinResolvedProxy(t, "")
	var calls atomic.Int32
	var lastDone, lastTotal int64
	err := downloadExtractOpts(context.Background(), bodyClient("0123456789", &calls), uniqueURL(t),
		Options{Progress: func(done, total int64) { lastDone, lastTotal = done, total }},
		func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if lastDone != 10 || lastTotal != 10 {
		t.Errorf("末次进度应满格 10/10，got %d/%d", lastDone, lastTotal)
	}
}
