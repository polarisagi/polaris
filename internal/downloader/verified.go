package downloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// Options 是下载的可选行为；零值等价于原有的"只下载、不校验、不汇报进度"。
//
// 为什么存在：几百 MB 的模型归档此前下载后不做任何完整性校验（镜像或代理截断/篡改
// 都会被当成成功），也没有进度可供前端展示，用户只能面对一个无响应的按钮（ADR-0107）。
type Options struct {
	// SHA256 非空时，归档下载（或命中缓存）后做校验：不符则删除缓存并报错，
	// 绝不把未通过校验的归档交给解压。小写十六进制。
	SHA256 string
	// SizeHint 是预期归档字节数，仅在服务端不给 Content-Length 时作为进度总量。
	SizeHint int64
	// Progress 在下载过程中被节流回调（约 4Hz，且保证末次满格回调）；nil 不汇报。
	// total 为 0 表示总量未知。回调在下载 goroutine 中同步执行，必须快速返回。
	Progress func(done, total int64)
}

// progressInterval 是进度回调的最小间隔：太密会拖慢下载并淹没状态机的日志。
const progressInterval = 250 * time.Millisecond

// progressSink 把 io 层的字节增量节流后转给 Options.Progress。
type progressSink struct {
	fn   func(done, total int64)
	hint int64

	mu    sync.Mutex
	done  int64
	total int64
	last  time.Time
}

func newProgressSink(o Options) *progressSink {
	if o.Progress == nil {
		return nil
	}
	return &progressSink{fn: o.Progress, hint: o.SizeHint}
}

// begin 在一次（续传）下载开始时设定起点与总量。
func (p *progressSink) begin(base, total int64) {
	p.mu.Lock()
	p.done, p.total = base, total
	p.last = time.Time{}
	p.mu.Unlock()
	p.emit(true)
}

func (p *progressSink) add(n int64) {
	p.mu.Lock()
	p.done += n
	p.mu.Unlock()
	p.emit(false)
}

func (p *progressSink) flush() { p.emit(true) }

func (p *progressSink) emit(force bool) {
	p.mu.Lock()
	if !force && time.Since(p.last) < progressInterval {
		p.mu.Unlock()
		return
	}
	p.last = time.Now()
	done, total := p.done, p.total
	p.mu.Unlock()
	p.fn(done, total)
}

// countWriter 统计写入字节数并汇报进度。
type countWriter struct {
	w    io.Writer
	sink *progressSink
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	if n > 0 {
		c.sink.add(int64(n))
	}
	return n, err //nolint:wrapcheck // io.Writer 语义：原样返回底层写错误
}

// downloadExtractOpts 下载归档到临时目录（断点续传），按 opts 校验 sha256 后提取。
// 提取成功后删除归档；提取失败保留归档（已通过校验），下次重试无需重新下载。
func downloadExtractOpts(ctx context.Context, client *http.Client, rawURL string, opts Options, extract func(string) error) error {
	archiveName := urlBaseName(rawURL)
	archivePath := filepath.Join(os.TempDir(), "polaris-dl-"+archiveName)

	mu := getDlLock(archivePath)
	mu.Lock()
	defer mu.Unlock()

	// 缓存命中且校验失败 → 视为陈旧/损坏缓存，删掉重下一次；
	// 刚下载完仍校验失败 → 源头问题，直接报错，重下只会得到同样的字节。
	for attempt := 0; ; attempt++ {
		_, statErr := os.Stat(archivePath)
		cached := statErr == nil

		if err := downloadResumeP(ctx, client, rawURL, archivePath, newProgressSink(opts)); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "downloadExtract", err)
		}
		if opts.SHA256 == "" {
			break
		}
		got, err := fileSHA256(archivePath)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "downloadExtract: sha256 计算失败", err)
		}
		if got == opts.SHA256 {
			break
		}
		if rmErr := os.Remove(archivePath); rmErr != nil {
			slog.Warn("downloader: 删除校验失败的归档失败", "path", archivePath, "err", rmErr)
		}
		if cached && attempt == 0 {
			slog.Warn("downloader: 缓存归档 sha256 不符，已删除并重新下载",
				"file", archiveName, "want", opts.SHA256, "got", got)
			continue
		}
		return apperr.New(apperr.CodeInternal, fmt.Sprintf(
			"downloader: %s sha256 校验失败（期望 %s，实得 %s）；归档已删除，可能被镜像/代理截断或篡改",
			archiveName, opts.SHA256, got))
	}

	if err := extract(archivePath); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "downloadExtract", err)
	}
	if err := os.Remove(archivePath); err != nil {
		slog.Warn("downloader: 删除已解压归档失败", "path", archivePath, "err", err)
	}
	return nil
}

// fileSHA256 流式计算文件 sha256（小写十六进制），避免把数百 MB 归档读进内存。
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "downloader: 打开文件计算 sha256 失败", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "downloader: 读取文件计算 sha256 失败", err)
	}
	return strings.ToLower(hex.EncodeToString(h.Sum(nil))), nil
}

// DownloadFileOpts 将 rawURL 内容写入 destPath，支持断点续传与 sha256 校验。
// 若文件已存在且 sha256 匹配，直接返回；若不匹配则删除重下；下载后仍不匹配则删除并报错。
func DownloadFileOpts(ctx context.Context, client *http.Client, rawURL, destPath string, opts Options) error {
	mu := getDlLock(destPath)
	mu.Lock()
	defer mu.Unlock()

	for attempt := 0; ; attempt++ {
		_, statErr := os.Stat(destPath)
		cached := statErr == nil
		if cached && opts.SHA256 != "" {
			got, err := fileSHA256(destPath)
			if err == nil && got == opts.SHA256 {
				return nil
			}
			if rmErr := os.Remove(destPath); rmErr != nil {
				slog.Debug("downloader: 删除旧缓存文件失败", "path", destPath, "err", rmErr)
			}
		}

		if err := downloadResumeP(ctx, client, rawURL, destPath, newProgressSink(opts)); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "DownloadFileOpts", err)
		}
		if opts.SHA256 == "" {
			break
		}
		got, err := fileSHA256(destPath)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "DownloadFileOpts: sha256 计算失败", err)
		}
		if got == opts.SHA256 {
			break
		}
		if rmErr := os.Remove(destPath); rmErr != nil {
			slog.Warn("downloader: 删除校验失败的文件失败", "path", destPath, "err", rmErr)
		}
		if cached && attempt == 0 {
			slog.Warn("downloader: 缓存文件 sha256 不符，已删除并重新下载",
				"file", filepath.Base(destPath), "want", opts.SHA256, "got", got)
			continue
		}
		return apperr.New(apperr.CodeInternal, fmt.Sprintf(
			"downloader: %s sha256 校验失败（期望 %s，实得 %s）；文件已删除，可能被镜像/代理截断或篡改",
			filepath.Base(destPath), opts.SHA256, got))
	}
	return nil
}

// DownloadExtractTarGzOpts 同 DownloadExtractTarBz2Opts，针对 .tar.gz / .tgz 归档。
func DownloadExtractTarGzOpts(ctx context.Context, client *http.Client, rawURL, destDir string, mapper func(string) (string, bool), opts Options) error {
	return downloadExtractOpts(ctx, client, rawURL, opts, func(path string) error {
		f, err := os.Open(path)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "DownloadExtractTarGz", err)
		}
		defer f.Close()
		return ExtractTarGz(f, destDir, mapper)
	})
}

// DownloadExtractZipOpts 同 DownloadExtractTarBz2Opts，针对 .zip 归档。
func DownloadExtractZipOpts(ctx context.Context, client *http.Client, rawURL, destDir string, mapper func(string) (string, bool), opts Options) error {
	return downloadExtractOpts(ctx, client, rawURL, opts, func(path string) error {
		return ExtractZip(path, destDir, mapper)
	})
}
