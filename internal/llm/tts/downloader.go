// Package tts 提供本地 TTS 模型（sherpa-onnx Kokoro）的资产下载与路径管理。
package tts

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/downloader"
	"github.com/polarisagi/polaris/internal/llm/audioassets"
	"github.com/polarisagi/polaris/internal/llm/stt"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ModelDir 返回 TTS 模型目录（ttsDir/model）。
func ModelDir(ttsDir string) string { return filepath.Join(ttsDir, "model") }

// requiredFiles 是 Kokoro 引擎运行必需的归档内文件（相对模型目录）；末尾带 "/" 的是目录。
// 缺任何一项引擎都会创建失败或读错音（espeak-ng-data 缺失→英文词无法发音）。
func requiredFiles() []string {
	return []string{
		"model.onnx", "voices.bin", "tokens.txt", "espeak-ng-data/",
		"lexicon-zh.txt", "lexicon-us-en.txt",
	}
}

// EnsureAssets 确保 libDir 下存在 sherpa-onnx 动态库、ttsDir/model 下存在完整 Kokoro 模型，幂等。
// 动态库与 STT 共用，优先复用 libDir（通常 = sttDir）；中国大陆网络自动走 ghproxy。
// 全部下载经清单 sha256 校验，进度经 progress 汇报（nil 安全）。
//
//   - version: sherpa-onnx 版本；空则取 stt.SherpaABIVersion，与之不同则报错
func EnsureAssets(ctx context.Context, libDir, ttsDir string, httpClient *http.Client, version string, progress audioassets.ProgressFunc) error {
	// 版本与 STT 共用同一套 ABI 钉死校验：不一致即报错，不下载、不加载。
	if _, err := stt.ResolveSherpaVersion(version); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "tts: sherpa version check failed", err)
	}
	if err := os.MkdirAll(ttsDir, 0o755); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "tts: mkdir "+ttsDir+" failed", err)
	}

	// ── 1. sherpa-onnx 动态库（复用 STT 目录，幂等） ─────────────────────────
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "tts: mkdir "+libDir+" failed", err)
	}
	if err := stt.EnsureLib(ctx, libDir, httpClient, progress); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "tts: library ensure failed", err)
	}

	// ── 2. TTS 模型文件 ────────────────────────────────────────────────────────
	asset := audioassets.KokoroModel()
	modelDir := ModelDir(ttsDir)
	if ModelMissing(modelDir) == "" {
		return nil
	}
	slog.Info("tts: downloading TTS model", "dest", modelDir, "asset", asset.File, "bytes", asset.Size)
	if err := downloader.DownloadExtractTarBz2Opts(ctx, httpClient, asset.URL(), modelDir,
		ttsModelMapper(modelDir), stt.AssetOptions(asset, progress)); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "tts: model download failed", err)
	}
	// 解压后复核必需文件：归档结构与预期不符时 extractTar 仍可能因写出了其他文件而判成功。
	if miss := ModelMissing(modelDir); miss != "" {
		return apperr.New(apperr.CodeInternal, "tts: 归档 "+asset.File+" 解压后缺少必需文件 "+miss+"（目录 "+modelDir+"）")
	}
	slog.Info("tts: model ready", "dir", modelDir)
	return nil
}

// ModelMissing 返回模型目录中第一个缺失的必需文件（相对路径）；齐备返回 ""。
func ModelMissing(modelDir string) string {
	for _, rel := range requiredFiles() {
		isDir := strings.HasSuffix(rel, "/")
		fi, err := os.Stat(filepath.Join(modelDir, strings.TrimSuffix(rel, "/")))
		if err != nil || fi.IsDir() != isDir {
			return rel
		}
	}
	return ""
}

// Installed 报告 TTS 资产（动态库 + 完整 Kokoro 模型）是否齐备。
// 只看文件存在性：已解压文件无法再对归档 sha256 复验，完整性由下载时的校验保证。
func Installed(libDir, ttsDir string) bool {
	return len(MissingAssets(libDir, ttsDir)) == 0
}

// MissingAssets 返回缺失的清单项（顺序：库、Kokoro 模型）。平台无库清单时不计库。
func MissingAssets(libDir, ttsDir string) []audioassets.Asset {
	var missing []audioassets.Asset
	if lib, err := stt.LibAssetForHost(); err == nil {
		if _, statErr := os.Stat(filepath.Join(libDir, stt.LibName())); statErr != nil {
			missing = append(missing, lib)
		}
	}
	if ModelMissing(ModelDir(ttsDir)) != "" {
		missing = append(missing, audioassets.KokoroModel())
	}
	return missing
}

// ttsModelMapper 返回 Kokoro 归档的 mapper：保留归档内完整目录结构，只剥掉顶层目录。
// 为什么不再按文件名白名单挑文件：v1.1 需要 espeak-ng-data/、dict/、多份 lexicon 与 fst，
// 白名单漏一项就是"能加载但读错音"这类难排查的缺陷；全量保留最不易错（归档里没有可执行文件）。
func ttsModelMapper(modelDir string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		clean := path.Clean(strings.TrimPrefix(name, "./"))
		_, rel, ok := strings.Cut(clean, "/")
		if !ok || rel == "" || rel == "." {
			return "", false // 顶层散文件或目录项本身：无顶层目录可剥，丢弃
		}
		// 剥掉顶层后仍以 ".." 开头（如 "a/../../b"）或为绝对路径即属逃逸，直接丢弃；
		// extractTar 另有一道前缀校验，这里是第一道，不依赖下游兜底。
		if rel == ".." || strings.HasPrefix(rel, "../") || strings.HasPrefix(clean, "/") {
			return "", false
		}
		return filepath.Join(modelDir, filepath.FromSlash(rel)), true
	}
}
