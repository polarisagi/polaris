package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// downloadChunk 向 url 发起 Range GET，将响应体写入 partPath。
// offset>0 时携带 Range 头；服务端返回 206 则追加，返回 200 则覆写（服务端不支持 Range）。
func downloadChunk(ctx context.Context, client *http.Client, url, partPath string, offset int64) error {
	return downloadChunkP(ctx, client, url, partPath, offset, nil)
}

// downloadChunkP 是 downloadChunk 的带进度版本；prog 为 nil 时行为与原版完全一致。
func downloadChunkP(ctx context.Context, client *http.Client, url, partPath string, offset int64, prog *progressSink) error {
	if client == nil {
		return apperr.New(apperr.CodeInternal, "downloader: http.Client is required; use substrate.NewSafeHTTPClient")
	}
	c := client
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "downloader: build request failed", err)
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := c.Do(req)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "downloader: GET "+url+" failed", err)
	}
	defer resp.Body.Close()

	var flags int
	switch resp.StatusCode {
	case http.StatusPartialContent: // 206：服务端支持 Range，追加
		flags = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	case http.StatusOK: // 200：不支持 Range，重新完整下载
		flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	default:
		return apperr.New(apperr.CodeInternal, "downloader: HTTP "+fmt.Sprint(resp.StatusCode)+" for "+url)
	}

	f, err := os.OpenFile(partPath, flags, 0o644)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "downloader: open part file failed", err)
	}
	defer f.Close()

	var dst io.Writer = f
	if prog != nil {
		// 206 追加：已有 offset 字节；200 覆写：从 0 重来。总量取响应声明的长度，
		// 缺失（-1，如分块传输）时退回调用方给的提示值（清单里的字节数）。
		base := int64(0)
		if resp.StatusCode == http.StatusPartialContent {
			base = offset
		}
		total := prog.hint
		if resp.ContentLength > 0 {
			total = base + resp.ContentLength
		}
		prog.begin(base, total)
		dst = &countWriter{w: f, sink: prog}
	}
	if _, err := io.Copy(dst, resp.Body); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "downloader: write failed", err)
	}
	if prog != nil {
		prog.flush()
	}
	return nil
}

type partMeta struct {
	ETag          string `json:"etag"`
	LastModified  string `json:"last_modified"`
	ContentLength int64  `json:"content_length"`
}

// downloadResume 按候选地址顺序将 rawURL 下载到 destPath，支持跨源断点续传。
// 临时文件为 destPath+".part"；完成后原子重命名。
// 若 destPath 已存在则幂等返回。
func downloadResume(ctx context.Context, client *http.Client, rawURL, destPath string) error {
	return downloadResumeP(ctx, client, rawURL, destPath, nil)
}

// downloadResumeP 是 downloadResume 的带进度版本；prog 为 nil 时行为与原版完全一致。
//
//nolint:gocyclo
func downloadResumeP(ctx context.Context, client *http.Client, rawURL, destPath string, prog *progressSink) error {
	if _, err := os.Stat(destPath); err == nil {
		if prog != nil {
			prog.begin(prog.hint, prog.hint) // 缓存命中：无需下载，进度直接满格
			prog.flush()
		}
		return nil
	}

	partPath := destPath + ".part"
	metaPath := partPath + ".meta"

	candidates := CandidateURLs(ctx, client, rawURL)
	var lastErr error
	for _, url := range candidates {
		var currentMeta partMeta
		headReq, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
		if err == nil {
			if headResp, err := client.Do(headReq); err == nil {
				currentMeta.ETag = headResp.Header.Get("ETag")
				currentMeta.LastModified = headResp.Header.Get("Last-Modified")
				currentMeta.ContentLength = headResp.ContentLength
				headResp.Body.Close()
			}
		}

		var offset int64
		if fi, err := os.Stat(partPath); err == nil { //nolint:nestif
			offset = fi.Size()
			if offset > 0 {
				var meta partMeta
				metaData, metaErr := os.ReadFile(metaPath)
				if metaErr == nil {
					if unmarshalErr := json.Unmarshal(metaData, &meta); unmarshalErr != nil {
						slog.Debug("downloader: failed to parse meta file", "err", unmarshalErr)
					}
				}

				// 换源同样比对。无这些 header 的视为不一致。
				if meta.ETag == "" || meta.LastModified == "" ||
					meta.ETag != currentMeta.ETag ||
					meta.LastModified != currentMeta.LastModified ||
					meta.ContentLength != currentMeta.ContentLength {
					metrics.RecordDownloaderResumeRestart(ctx)
					os.Remove(partPath)
					os.Remove(metaPath)
					offset = 0
				} else {
					slog.Info("downloader: resuming partial download", "file", lastSegment(destPath), "offset_bytes", offset)
				}
			}
		}

		if offset == 0 {
			b, _ := json.Marshal(currentMeta)
			if writeErr := os.WriteFile(metaPath, b, 0644); writeErr != nil {
				slog.Debug("downloader: failed to write meta file", "err", writeErr)
			}
		}

		if err := downloadChunkP(ctx, client, url, partPath, offset, prog); err != nil {
			slog.Warn("downloader: source failed, trying fallback", "url", url, "err", err)
			lastErr = err
			continue
		}
		if err := os.Rename(partPath, destPath); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "downloader: rename downloaded file failed", err)
		}
		os.Remove(metaPath)
		return nil
	}
	return apperr.Wrap(apperr.CodeInternal, "downloader: all sources failed", lastErr)
}

// getDlLock 返回 destPath 专属的互斥锁，防止并发重复下载同一文件。
// 使用 sync.OnceValue 惰性初始化封闭对象，避免包级裸可变变量（R1.3）。
//
//nolint:gochecknoglobals // 故意用全局 OnceValue 实现按路径加锁
var getDlLockState = sync.OnceValue(func() *sync.Map { return &sync.Map{} })

func getDlLock(destPath string) *sync.Mutex {
	v, _ := getDlLockState().LoadOrStore(destPath, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// downloadExtract 下载归档到临时目录（支持断点续传），完成后提取。
// 提取成功后删除归档；提取失败保留归档，下次重试时无需重新下载。
func downloadExtract(ctx context.Context, client *http.Client, rawURL string, extract func(string) error) error {
	return downloadExtractOpts(ctx, client, rawURL, Options{}, extract)
}

// DownloadFile 将 rawURL 内容写入 destPath，支持断点续传。
// 按 ghproxy.net → mirror.ghproxy.com → 直连顺序降级。
func DownloadFile(ctx context.Context, client *http.Client, rawURL, destPath string) error {
	return downloadResume(ctx, client, rawURL, destPath)
}

// DownloadExtractTarBz2 下载 .tar.bz2 并调用 mapper 选择性提取，支持断点续传。
func DownloadExtractTarBz2(ctx context.Context, client *http.Client, rawURL string, destDir string, mapper func(string) (string, bool)) error {
	return DownloadExtractTarBz2Opts(ctx, client, rawURL, destDir, mapper, Options{})
}

// DownloadExtractTarBz2Opts 同 DownloadExtractTarBz2，附带 sha256 校验与进度回调（见 Options）。
func DownloadExtractTarBz2Opts(ctx context.Context, client *http.Client, rawURL string, destDir string, mapper func(string) (string, bool), opts Options) error {
	return downloadExtractOpts(ctx, client, rawURL, opts, func(path string) error {
		f, err := os.Open(path)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "DownloadExtractTarBz2", err)
		}
		defer f.Close()
		return ExtractTarBz2(f, destDir, mapper)
	})
}

// DownloadExtractLibs 下载动态库压缩包，将所有 .so/.dylib/.dll 提取到 destDir，支持断点续传。
func DownloadExtractLibs(ctx context.Context, client *http.Client, rawURL, destDir string) error {
	return DownloadExtractLibsOpts(ctx, client, rawURL, destDir, Options{})
}

// DownloadExtractLibsOpts 同 DownloadExtractLibs，附带 sha256 校验与进度回调（见 Options）。
func DownloadExtractLibsOpts(ctx context.Context, client *http.Client, rawURL, destDir string, opts Options) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "downloader: mkdir "+destDir+" failed", err)
	}
	return DownloadExtractTarBz2Opts(ctx, client, rawURL, destDir, func(name string) (string, bool) {
		base := lastSegment(name)
		if strings.HasSuffix(base, ".dylib") || strings.HasSuffix(base, ".so") ||
			strings.HasSuffix(base, ".dll") {
			return joinPath(destDir, base), true
		}
		return "", false
	}, opts)
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
