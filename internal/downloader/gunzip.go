package downloader

import (
	"compress/gzip"
	"context"
	"net/http"
	"os"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// DownloadGunzip 下载单个 .gz 文件（支持断点续传与代理加速），解压并原子写入 destPath。
// 主要用于 ffmpeg-static 等单一二进制压缩包的自动化部署。
func DownloadGunzip(ctx context.Context, client *http.Client, rawURL, destPath string) error {
	return downloadExtract(ctx, client, rawURL, func(archivePath string) error {
		f, err := os.Open(archivePath)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "downloader: open gzip archive failed", err)
		}
		defer f.Close()

		gz, err := gzip.NewReader(f)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "downloader: open gzip reader failed", err)
		}
		defer gz.Close()

		if err := writeFromReader(gz, destPath, 0o755); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "downloader: write uncompressed file failed", err)
		}
		return nil
	})
}
