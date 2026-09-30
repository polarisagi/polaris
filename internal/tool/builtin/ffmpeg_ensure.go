package builtin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/polarisagi/polaris/internal/downloader"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// FFmpegStaticURL 返回当前操作系统与架构对应的 ffmpeg 静态编译二进制下载地址。
func FFmpegStaticURL() (string, error) {
	switch {
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		return "https://github.com/eugeneware/ffmpeg-static/releases/download/b6.0/ffmpeg-darwin-arm64.gz", nil
	case runtime.GOOS == "darwin" && runtime.GOARCH == "amd64":
		return "https://github.com/eugeneware/ffmpeg-static/releases/download/b6.0/ffmpeg-darwin-x64.gz", nil
	case runtime.GOOS == "linux" && runtime.GOARCH == "amd64":
		return "https://github.com/eugeneware/ffmpeg-static/releases/download/b6.0/ffmpeg-linux-x64.gz", nil
	case runtime.GOOS == "linux" && runtime.GOARCH == "arm64":
		return "https://github.com/eugeneware/ffmpeg-static/releases/download/b6.0/ffmpeg-linux-arm64.gz", nil
	case runtime.GOOS == "windows" && runtime.GOARCH == "amd64":
		return "https://github.com/eugeneware/ffmpeg-static/releases/download/b6.0/ffmpeg-win32-x64.gz", nil
	default:
		return "", apperr.New(apperr.CodeInternal,
			fmt.Sprintf("ffmpeg: unsupported platform %s/%s", runtime.GOOS, runtime.GOARCH))
	}
}

// EnsureFFmpeg 按三级优先级查找或自动安装 ffmpeg 可执行文件并返回其路径：
//  1. binDir 目录下已安装的 ffmpeg (如 ~/.polarisagi/polaris/bin/ffmpeg)
//  2. 系统全局 PATH 中的 ffmpeg
//  3. 若均不存在且提供了 httpClient 与 binDir，则自动从 GitHub 下载静态编译包解压安装
//
// 安装失败或未安装时返回错误，供调用方决定明确回退或报错，禁止静默忽略。
func EnsureFFmpeg(ctx context.Context, binDir string, httpClient *http.Client) (string, error) {
	exeName := "ffmpeg"
	if runtime.GOOS == "windows" {
		exeName = "ffmpeg.exe"
	}

	// 1. 检查 binDir
	if binDir != "" {
		binPath := filepath.Join(binDir, exeName)
		if fi, err := os.Stat(binPath); err == nil && !fi.IsDir() {
			return binPath, nil
		}
	}

	// 2. 检查系统全局 PATH
	if sysPath, err := exec.LookPath(exeName); err == nil {
		return sysPath, nil
	}

	// 3. 自动安装到 binDir
	if binDir == "" {
		return "", apperr.New(apperr.CodeNotFound, "ffmpeg not found in PATH and binDir is empty, auto-install skipped")
	}
	if httpClient == nil {
		return "", apperr.New(apperr.CodeNotFound, "ffmpeg not found and httpClient is nil, cannot download")
	}

	destPath := filepath.Join(binDir, exeName)
	// 二次检查（防止重复下载）
	if fi, err := os.Stat(destPath); err == nil && !fi.IsDir() {
		return destPath, nil
	}

	dlURL, err := FFmpegStaticURL()
	if err != nil {
		return "", err
	}

	slog.Info("ffmpeg: binary not found, starting silent download...", "dest", destPath, "url", dlURL)
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "ffmpeg: failed to create binDir", err)
	}

	if err := downloader.DownloadGunzip(ctx, httpClient, dlURL, destPath); err != nil {
		slog.Warn("ffmpeg: auto-install failed", "err", err)
		return "", apperr.Wrap(apperr.CodeInternal, "ffmpeg: auto-install download failed", err)
	}

	// 赋予执行权限
	if runtime.GOOS != "windows" {
		_ = os.Chmod(destPath, 0o755)
	}

	slog.Info("ffmpeg: installed successfully", "path", destPath)
	return destPath, nil
}
