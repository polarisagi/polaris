package downloader

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Get 发起带 context 的 GET 请求，非 200 视为错误。
// client 必须由调用方注入（通常是 substrate.NewSafeHTTPClient），禁止传 nil。
func Get(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	if client == nil {
		return nil, fmt.Errorf("downloader: http.Client is required; use substrate.NewSafeHTTPClient")
	}
	c := client
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("downloader: build request: %w", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloader: GET %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("downloader: HTTP %d for %s", resp.StatusCode, url)
	}
	return resp, nil
}

// downloadChunk 向 url 发起 Range GET，将响应体写入 partPath。
// offset>0 时携带 Range 头；服务端返回 206 则追加，返回 200 则覆写（服务端不支持 Range）。
func downloadChunk(ctx context.Context, client *http.Client, url, partPath string, offset int64) error {
	if client == nil {
		return fmt.Errorf("downloader: http.Client is required; use substrate.NewSafeHTTPClient")
	}
	c := client
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("downloader: build request: %w", err)
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("downloader: GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	var flags int
	switch resp.StatusCode {
	case http.StatusPartialContent: // 206：服务端支持 Range，追加
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	case http.StatusOK: // 200：不支持 Range，重新完整下载
		flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	default:
		return fmt.Errorf("downloader: HTTP %d for %s", resp.StatusCode, url)
	}

	f, err := os.OpenFile(partPath, flags, 0o644)
	if err != nil {
		return fmt.Errorf("downloader: open part file: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("downloader: write: %w", err)
	}
	return nil
}

// downloadResume 按候选地址顺序将 rawURL 下载到 destPath，支持跨源断点续传。
// 临时文件为 destPath+".part"；完成后原子重命名。
// 若 destPath 已存在则幂等返回。
func downloadResume(ctx context.Context, client *http.Client, rawURL, destPath string) error {
	if _, err := os.Stat(destPath); err == nil {
		return nil
	}

	partPath := destPath + ".part"

	var offset int64
	if fi, err := os.Stat(partPath); err == nil {
		offset = fi.Size()
		if offset > 0 {
			slog.Info("downloader: resuming partial download",
				"file", lastSegment(destPath), "offset_bytes", offset)
		}
	}

	candidates := CandidateURLs(ctx, client, rawURL)
	var lastErr error
	for _, url := range candidates {
		if err := downloadChunk(ctx, client, url, partPath, offset); err != nil {
			slog.Warn("downloader: source failed, trying fallback", "url", url, "err", err)
			// 更新 offset：本次可能已写入部分字节
			if fi, statErr := os.Stat(partPath); statErr == nil {
				offset = fi.Size()
			}
			lastErr = err
			continue
		}
		return os.Rename(partPath, destPath)
	}
	return fmt.Errorf("downloader: all %d sources failed, last: %w", len(candidates), lastErr)
}

// downloadExtract 下载归档到临时目录（支持断点续传），完成后提取。
// 提取成功后删除归档；提取失败保留归档，下次重试时无需重新下载。
func downloadExtract(ctx context.Context, client *http.Client, rawURL string, extract func(string) error) error {
	archiveName := urlBaseName(rawURL)
	archivePath := filepath.Join(os.TempDir(), "polaris-dl-"+archiveName)
	if err := downloadResume(ctx, client, rawURL, archivePath); err != nil {
		return err
	}
	if err := extract(archivePath); err != nil {
		return err
	}
	os.Remove(archivePath) //nolint:errcheck
	return nil
}

// DownloadFile 将 rawURL 内容写入 destPath，支持断点续传。
// 按 ghproxy.net → mirror.ghproxy.com → 直连顺序降级。
func DownloadFile(ctx context.Context, client *http.Client, rawURL, destPath string) error {
	return downloadResume(ctx, client, rawURL, destPath)
}

// DownloadExtractTarBz2 下载 .tar.bz2 并调用 mapper 选择性提取，支持断点续传。
func DownloadExtractTarBz2(ctx context.Context, client *http.Client, rawURL string, mapper func(string) (string, bool)) error {
	return downloadExtract(ctx, client, rawURL, func(path string) error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return ExtractTarBz2(f, mapper)
	})
}

// DownloadExtractTarGz 下载 .tar.gz 并调用 mapper 选择性提取，支持断点续传。
func DownloadExtractTarGz(ctx context.Context, client *http.Client, rawURL string, mapper func(string) (string, bool)) error {
	return downloadExtract(ctx, client, rawURL, func(path string) error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return ExtractTarGz(f, mapper)
	})
}

// DownloadExtractLibs 下载动态库压缩包，将所有 .so/.dylib/.dll 提取到 destDir，支持断点续传。
func DownloadExtractLibs(ctx context.Context, client *http.Client, rawURL, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("downloader: mkdir %s: %w", destDir, err)
	}
	return DownloadExtractTarBz2(ctx, client, rawURL, func(name string) (string, bool) {
		base := lastSegment(name)
		if strings.HasSuffix(base, ".dylib") || strings.HasSuffix(base, ".so") ||
			strings.HasSuffix(base, ".dll") {
			return joinPath(destDir, base), true
		}
		return "", false
	})
}

// urlBaseName 从 URL 中提取文件名（去掉查询参数和 fragment）。
func urlBaseName(rawURL string) string {
	u := rawURL
	if i := strings.IndexByte(u, '?'); i >= 0 {
		u = u[:i]
	}
	if i := strings.IndexByte(u, '#'); i >= 0 {
		u = u[:i]
	}
	return lastSegment(u)
}

// lastSegment 返回路径最后一段（等价于 filepath.Base，避免额外导入）。
func lastSegment(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[i+1:]
		}
	}
	return p
}

func joinPath(dir, base string) string {
	return dir + "/" + base
}
