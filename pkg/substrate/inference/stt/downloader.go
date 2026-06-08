package stt

import (
	"archive/tar"
	"compress/bzip2"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// modelRequiredFiles 是模型目录下必须存在的文件。
var modelRequiredFiles = []string{"model.onnx", "tokens.txt"}

// LibName 返回当前平台的动态库文件名（供 LoadLibrary 使用）。
func LibName() string {
	if runtime.GOOS == "darwin" {
		return "libsherpa-onnx-c-api.dylib"
	}
	return "libsherpa-onnx-c-api.so"
}

// libDownloadURL 返回当前 OS/ARCH 对应的预编译库下载地址。
func libDownloadURL(version string) (string, error) {
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
	default:
		return "", fmt.Errorf("unsupported platform: %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	return fmt.Sprintf(
		"https://github.com/k2-fsa/sherpa-onnx/releases/download/v%s/sherpa-onnx-v%s-%s-shared.tar.bz2",
		version, version, platform,
	), nil
}

// EnsureAssets 确保 sttDir 下存在可用的动态库与模型文件。
// 缺失时通过 httpClient（SafeHTTPClient）从 GitHub Releases 流式下载并解压，幂等。
func EnsureAssets(ctx context.Context, sttDir string, httpClient *http.Client, version, modelURL, punctModelURL string) error {
	if err := os.MkdirAll(sttDir, 0o755); err != nil {
		return fmt.Errorf("stt: mkdir %s: %w", sttDir, err)
	}

	// ── 1. 原生动态库 ────────────────────────────────────────────────────────
	libPath := filepath.Join(sttDir, LibName())
	if _, err := os.Stat(libPath); os.IsNotExist(err) {
		url, err := libDownloadURL(version)
		if err != nil {
			return fmt.Errorf("stt: %w", err)
		}
		slog.Info("stt: downloading sherpa-onnx library", "url", url, "dest", libPath)
		if err := downloadExtractLib(ctx, httpClient, url, sttDir, LibName()); err != nil {
			return fmt.Errorf("stt: library download: %w", err)
		}
		slog.Info("stt: library ready", "path", libPath)
	} else {
		slog.Info("stt: library already present, skipping download", "path", libPath)
	}

	// ── 2. SenseVoice 模型文件 ───────────────────────────────────────────────
	modelDir := filepath.Join(sttDir, "model")
	if !modelFilesPresent(modelDir) {
		slog.Info("stt: downloading SenseVoice model", "url", modelURL, "dest", modelDir)
		if err := downloadExtractModel(ctx, httpClient, modelURL, modelDir); err != nil {
			return fmt.Errorf("stt: model download: %w", err)
		}
		slog.Info("stt: model ready", "dir", modelDir)
	} else {
		slog.Info("stt: model already present, skipping download", "dir", modelDir)
	}

	// ── 3. 标点模型文件 ───────────────────────────────────────────────
	if punctModelURL != "" {
		punctDir := filepath.Join(sttDir, "punct_model")
		if _, err := os.Stat(filepath.Join(punctDir, "model.onnx")); os.IsNotExist(err) {
			slog.Info("stt: downloading punctuation model", "url", punctModelURL, "dest", punctDir)
			// Punctuation model only needs model.onnx
			if err := downloadExtractModel(ctx, httpClient, punctModelURL, punctDir); err != nil {
				return fmt.Errorf("stt: punctuation model download: %w", err)
			}
			slog.Info("stt: punctuation model ready", "dir", punctDir)
		} else {
			slog.Info("stt: punctuation model already present, skipping download", "dir", punctDir)
		}
	}

	return nil
}

// ModelDir 返回模型目录（sttDir/model），供 NewEngine 使用。
func ModelDir(sttDir string) string {
	return filepath.Join(sttDir, "model")
}

// PunctModelDir 返回标点模型目录（sttDir/punct_model），供 NewEngine 使用。
func PunctModelDir(sttDir string) string {
	return filepath.Join(sttDir, "punct_model")
}

// modelFilesPresent 检查所有必要模型文件是否已存在。
func modelFilesPresent(modelDir string) bool {
	for _, f := range modelRequiredFiles {
		if _, err := os.Stat(filepath.Join(modelDir, f)); os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// downloadExtractLib 下载 .tar.bz2 并仅提取目标动态库文件（扁平输出到 destDir）。
func downloadExtractLib(ctx context.Context, client *http.Client, url, destDir, libFilename string) error {
	resp, err := doGet(ctx, client, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return extractTarBz2(resp.Body, func(name string) (string, bool) {
		// tarball 内有 libsherpa-onnx-c-api 以及依赖的 libonnxruntime 等
		if strings.HasSuffix(name, ".dylib") || strings.HasSuffix(name, ".so") || strings.HasSuffix(name, ".dll") {
			return filepath.Join(destDir, filepath.Base(name)), true
		}
		return "", false
	})
}

// downloadExtractModel 下载模型 .tar.bz2 并提取 model.onnx / tokens.txt 到 modelDir。
func downloadExtractModel(ctx context.Context, client *http.Client, url, modelDir string) error {
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		return err
	}
	resp, err := doGet(ctx, client, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return extractTarBz2(resp.Body, func(name string) (string, bool) {
		base := filepath.Base(name)
		if base == "model.onnx" || base == "tokens.txt" {
			return filepath.Join(modelDir, base), true
		}
		return "", false
	})
}

// doGet 发起带 context 的 GET 请求，非 200 视为错误。
func doGet(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, url)
	}
	return resp, nil
}

// extractTarBz2 流式解压 .tar.bz2，对每个普通文件调用 mapper 决定是否写出及写入路径。
// mapper(tarEntryName) → (destAbsPath, shouldWrite)
func extractTarBz2(r io.Reader, mapper func(string) (string, bool)) error {
	bzr := bzip2.NewReader(r)
	tr := tar.NewReader(bzr)
	written := 0

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("stt: tar read: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		destPath, ok := mapper(hdr.Name)
		if !ok {
			continue
		}
		if err := writeFromReader(tr, destPath, os.FileMode(hdr.Mode)|0o600); err != nil {
			return fmt.Errorf("stt: write %s: %w", destPath, err)
		}
		written++
	}

	if written == 0 {
		return fmt.Errorf("stt: no target files found in archive")
	}
	return nil
}

// writeFromReader 将 r 的内容原子写入 path（先写临时文件再 rename）。
func writeFromReader(r io.Reader, path string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(tmp) //nolint:errcheck
		return err
	}
	f.Close()
	return os.Rename(tmp, path)
}
