package stt

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"

	"github.com/polarisagi/polaris/internal/sysmgr/downloader"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// LibName 返回当前平台的 sherpa-onnx 动态库文件名（供 LoadLibrary 使用）。
func LibName() string {
	switch runtime.GOOS {
	case "darwin":
		return "libsherpa-onnx-c-api.dylib"
	case "windows":
		return "sherpa-onnx-c-api.dll"
	default:
		return "libsherpa-onnx-c-api.so"
	}
}

// SherpaLibURL 返回当前 OS/ARCH 对应的 sherpa-onnx 预编译库下载地址（GitHub Releases）。
// 导出供 tts 包复用，避免重复维护平台映射表。
func SherpaLibURL(version string) (string, error) {
	return libDownloadURL(version)
}

// libDownloadURL 是内部实现。
func libDownloadURL(version string) (string, error) {
	if version == "" {
		return "", apperr.New(apperr.CodeInternal,
			"stt: sherpa_version is empty; set llm.stt.sherpa_version in config.toml")
	}
	var platform string
	switch {
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		platform = "osx-arm64"
	case runtime.GOOS == "darwin" && runtime.GOARCH == "amd64":
		platform = "osx-x86_64"
	case runtime.GOOS == "linux" && runtime.GOARCH == "amd64":
		platform = "linux-x86_64"
	case runtime.GOOS == "linux" && runtime.GOARCH == "arm64":
		platform = "linux-aarch64"
	case runtime.GOOS == "windows" && runtime.GOARCH == "amd64":
		platform = "win-x64"
	default:
		return "", apperr.New(apperr.CodeInternal, "unsupported platform: "+runtime.GOOS+"/"+runtime.GOARCH)
	}
	return fmt.Sprintf(
		"https://github.com/k2-fsa/sherpa-onnx/releases/download/v%s/sherpa-onnx-v%s-%s-shared.tar.bz2",
		version, version, platform,
	), nil
}

// EnsureAssets 确保 sttDir 下存在可用的动态库与模型文件，幂等。
// 中国大陆网络下自动通过 ghproxy 加速下载。
func EnsureAssets(ctx context.Context, sttDir string, httpClient *http.Client, version, modelURL, punctModelURL string) error {
	if err := os.MkdirAll(sttDir, 0o755); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "stt: mkdir "+sttDir+" failed", err)
	}

	// ── 1. sherpa-onnx 动态库 ─────────────────────────────────────────────────
	libPath := filepath.Join(sttDir, LibName())
	if _, err := os.Stat(libPath); os.IsNotExist(err) {
		rawURL, err := libDownloadURL(version)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "stt: download sherpa-onnx failed", err)
		}
		slog.Info("stt: downloading sherpa-onnx library", "dest", libPath)
		if err := downloader.DownloadExtractLibs(ctx, httpClient, rawURL, sttDir); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "stt: library download failed", err)
		}
		slog.Info("stt: library ready", "path", libPath)
	} else {
		slog.Info("stt: library already present, skipping download", "path", libPath)
	}

	// ── 2. SenseVoice 模型文件 ───────────────────────────────────────────────
	modelDir := filepath.Join(sttDir, "model")
	if !modelFilesPresent(modelDir) {
		slog.Info("stt: downloading SenseVoice model", "dest", modelDir)
		if err := downloader.DownloadExtractTarBz2(ctx, httpClient, modelURL, modelDir, sttModelMapper(modelDir)); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "stt: model download failed", err)
		}
		slog.Info("stt: model ready", "dir", modelDir)
	} else {
		slog.Info("stt: model already present, skipping download", "dir", modelDir)
	}

	// ── 3. 标点模型文件 ───────────────────────────────────────────────────────
	if punctModelURL != "" {
		punctDir := filepath.Join(sttDir, "punct_model")
		if _, err := os.Stat(filepath.Join(punctDir, "model.onnx")); os.IsNotExist(err) {
			slog.Info("stt: downloading punctuation model", "dest", punctDir)
			if err := downloader.DownloadExtractTarBz2(ctx, httpClient, punctModelURL, punctDir, sttModelMapper(punctDir)); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "stt: punctuation model download failed", err)
			}
			slog.Info("stt: punctuation model ready", "dir", punctDir)
		} else {
			slog.Info("stt: punctuation model already present, skipping download", "dir", punctDir)
		}
	}

	return nil
}

// sttModelMapper 返回只提取 model.onnx / tokens.txt 的 mapper 函数。
func sttModelMapper(modelDir string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		base := filepath.Base(name)
		if base == "model.onnx" || base == "tokens.txt" {
			return filepath.Join(modelDir, base), true
		}
		return "", false
	}
}

// modelFilesPresent 检查 STT 模型目录下必要文件是否已存在。
func modelFilesPresent(modelDir string) bool {
	// 必要文件列表（只读，仅在此处使用）
	required := []string{"model.onnx", "tokens.txt"}
	for _, f := range required {
		if _, err := os.Stat(filepath.Join(modelDir, f)); os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// ModelDir 返回 STT 模型目录（sttDir/model）。
func ModelDir(sttDir string) string { return filepath.Join(sttDir, "model") }

// PunctModelDir 返回标点模型目录（sttDir/punct_model）。
func PunctModelDir(sttDir string) string { return filepath.Join(sttDir, "punct_model") }
