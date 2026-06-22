// Package tts 提供本地 TTS 模型（sherpa-onnx）的资产下载与路径管理。
package tts

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/llm/stt"
	"github.com/polarisagi/polaris/internal/sysmgr/downloader"
	"github.com/polarisagi/polaris/pkg/apperr"
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
		return apperr.Wrap(apperr.CodeInternal, "tts: mkdir "+ttsDir+" failed", err)
	}

	// ── 1. sherpa-onnx 动态库（复用 STT 目录，幂等） ─────────────────────────
	libPath := filepath.Join(libDir, stt.LibName())
	if _, err := os.Stat(libPath); os.IsNotExist(err) {
		libURL, err := stt.SherpaLibURL(version)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "tts: download sherpa-onnx failed", err)
		}
		slog.Info("tts: downloading sherpa-onnx library", "dest", libPath)
		if err := downloader.DownloadExtractLibs(ctx, httpClient, libURL, libDir); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "tts: library download failed", err)
		}
		slog.Info("tts: library ready", "path", libPath)
	}

	// ── 2. TTS 模型文件 ────────────────────────────────────────────────────────
	modelDir := ModelDir(ttsDir)
	if !ttsModelPresent(modelDir) {
		slog.Info("tts: downloading TTS model", "dest", modelDir)
		if err := downloader.DownloadExtractTarBz2(ctx, httpClient, modelURL, modelDir, ttsModelMapper(modelDir)); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "tts: model download failed", err)
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

func ttsModelMapper(modelDir string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		// Kokoro 等模型依赖 espeak-ng-data，必须保留完整的子目录结构
		if idx := strings.Index(name, "espeak-ng-data"); idx != -1 {
			relPath := name[idx:]
			return filepath.Join(modelDir, relPath), true
		}

		base := filepath.Base(name)
		switch {
		case strings.HasSuffix(base, ".onnx"),
			strings.HasSuffix(base, ".bin"),
			base == "tokens.txt",
			strings.HasPrefix(base, "lexicon") && strings.HasSuffix(base, ".txt"),
			strings.HasSuffix(base, ".json"):
			return filepath.Join(modelDir, base), true
		}
		return "", false
	}
}
