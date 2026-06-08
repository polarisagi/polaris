// Package tts 提供本地 TTS 模型（sherpa-onnx）的资产下载与路径管理。
package tts

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/pkg/substrate/downloader"
	"github.com/polarisagi/polaris/pkg/substrate/inference/stt"
)

// ModelDir 返回 TTS 模型目录（ttsDir/model）。
func ModelDir(ttsDir string) string { return filepath.Join(ttsDir, "model") }

// EnsureAssets 确保 ttsDir 下存在可用的动态库与 TTS 模型文件，幂等。
// 动态库与 STT 共用，优先复用 libDir（通常 = sttDir）；中国大陆网络自动走 ghproxy。
//
//   - libDir:   sherpa-onnx 动态库目录（通常复用 STT 目录，避免重复下载）
//   - ttsDir:   TTS 专属目录，模型文件写入 ttsDir/model/
//   - version:  sherpa-onnx 版本（与 STT 保持一致）
//   - modelURL: kokoro / piper 等模型 .tar.bz2 下载地址（留空则跳过）
func EnsureAssets(ctx context.Context, libDir, ttsDir string, httpClient *http.Client, version, modelURL string) error {
	if modelURL == "" {
		return nil // 未配置本地 TTS，跳过（继续使用 edge-tts 云端 API）
	}
	if err := os.MkdirAll(ttsDir, 0o755); err != nil {
		return fmt.Errorf("tts: mkdir %s: %w", ttsDir, err)
	}

	// ── 1. sherpa-onnx 动态库（复用 STT 目录，幂等） ─────────────────────────
	libPath := filepath.Join(libDir, stt.LibName())
	if _, err := os.Stat(libPath); os.IsNotExist(err) {
		libURL, err := stt.SherpaLibURL(version)
		if err != nil {
			return fmt.Errorf("tts: %w", err)
		}
		slog.Info("tts: downloading sherpa-onnx library", "dest", libPath)
		if err := downloader.DownloadExtractLibs(ctx, httpClient, libURL, libDir); err != nil {
			return fmt.Errorf("tts: library download: %w", err)
		}
		slog.Info("tts: library ready", "path", libPath)
	}

	// ── 2. TTS 模型文件 ────────────────────────────────────────────────────────
	modelDir := ModelDir(ttsDir)
	if !ttsModelPresent(modelDir) {
		slog.Info("tts: downloading TTS model", "dest", modelDir)
		if err := downloader.DownloadExtractTarBz2(ctx, httpClient, modelURL, ttsModelMapper(modelDir)); err != nil {
			return fmt.Errorf("tts: model download: %w", err)
		}
		slog.Info("tts: model ready", "dir", modelDir)
	} else {
		slog.Info("tts: model already present, skipping download", "dir", modelDir)
	}

	return nil
}

// ttsModelPresent 检查 TTS 模型目录是否存在至少一个 .onnx 文件。
func ttsModelPresent(modelDir string) bool {
	entries, err := os.ReadDir(modelDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".onnx") {
			return true
		}
	}
	return false
}

// ttsModelMapper 提取 TTS 模型所需的所有组件文件（扁平输出到 modelDir）。
// kokoro / piper / Matcha-TTS 使用不同文件集合，按扩展名统一覆盖。
func ttsModelMapper(modelDir string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		base := filepath.Base(name)
		switch {
		case strings.HasSuffix(base, ".onnx"),
			strings.HasSuffix(base, ".bin"),
			base == "tokens.txt",
			base == "lexicon.txt",
			strings.HasSuffix(base, ".json"):
			return filepath.Join(modelDir, base), true
		}
		return "", false
	}
}
