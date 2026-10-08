// Package tts 提供本地 TTS 模型（sherpa-onnx MeloTTS / Matcha，ADR-0110）的资产下载、路径管理与合成。
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

// ModelDir 返回指定模型的目录（ttsDir/<model>）。
// 每个模型独占一个目录：B 档降级/用户指定模型时两套资产可并存，互不覆盖。
func ModelDir(ttsDir string, m Model) string { return filepath.Join(ttsDir, string(m)) }

// archiveAsset / vocoderAsset 返回模型对应的清单项；Matcha 之外的模型没有声码器。
func archiveAsset(m Model) audioassets.Asset {
	if m == ModelMatcha {
		return audioassets.MatchaModel()
	}
	return audioassets.MeloModel()
}

// EnsureAssets 确保 libDir 下存在 sherpa-onnx 动态库、ttsDir/<model> 下存在完整模型，幂等。
// 动态库与 STT 共用，优先复用 libDir（通常 = sttDir）；中国大陆网络自动走 ghproxy。
// 全部下载经清单 sha256 校验，进度经 progress 汇报（nil 安全）。
//
//   - version: sherpa-onnx 版本；空则取 stt.SherpaABIVersion，与之不同则报错
func EnsureAssets(ctx context.Context, libDir, ttsDir string, model Model, httpClient *http.Client, version string, progress audioassets.ProgressFunc) error {
	if _, err := ParseModel(string(model)); err != nil {
		return err
	}
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

	// ── 2. 模型归档 ───────────────────────────────────────────────────────────
	modelDir := ModelDir(ttsDir, model)
	if archiveMissing(modelDir, model) != "" {
		asset := archiveAsset(model)
		slog.Info("tts: downloading TTS model", "model", model, "dest", modelDir, "asset", asset.File, "bytes", asset.Size)
		if err := downloader.DownloadExtractTarBz2Opts(ctx, httpClient, asset.URL(), modelDir,
			ttsModelMapper(modelDir, model), stt.AssetOptions(asset, progress)); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "tts: model download failed", err)
		}
		// 解压后复核必需文件：归档结构与预期不符时 extractTar 仍可能因写出了其他文件而判成功。
		if miss := archiveMissing(modelDir, model); miss != "" {
			return apperr.New(apperr.CodeInternal, "tts: 归档 "+asset.File+" 解压后缺少必需文件 "+miss+"（目录 "+modelDir+"）")
		}
	}

	// ── 3. Matcha 声码器（单文件资产，非归档） ─────────────────────────────────
	if model == ModelMatcha {
		voc := audioassets.MatchaVocoder()
		dest := filepath.Join(modelDir, MatchaVocoderFile)
		if _, err := os.Stat(dest); err != nil {
			slog.Info("tts: downloading Matcha vocoder", "dest", dest, "bytes", voc.Size)
			if err := downloader.DownloadFileOpts(ctx, httpClient, voc.URL(), dest, stt.AssetOptions(voc, progress)); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "tts: vocoder download failed", err)
			}
		}
	}
	if miss := ModelMissing(modelDir, model); miss != "" {
		return apperr.New(apperr.CodeInternal, "tts: 模型 "+string(model)+" 安装后仍缺少 "+miss+"（目录 "+modelDir+"）")
	}
	slog.Info("tts: model ready", "model", model, "dir", modelDir)
	return nil
}

// ModelMissing 返回模型目录中第一个缺失的必需文件（相对路径）；齐备返回 ""。
func ModelMissing(modelDir string, m Model) string {
	return missingIn(modelDir, requiredFiles(m), true)
}

// archiveMissing 只检查来自归档的文件（不含单独下载的声码器）。
func archiveMissing(modelDir string, m Model) string {
	return missingIn(modelDir, requiredFiles(m), false)
}

func missingIn(modelDir string, files []string, withVocoder bool) string {
	for _, rel := range files {
		if !withVocoder && rel == MatchaVocoderFile {
			continue
		}
		isDir := strings.HasSuffix(rel, "/")
		fi, err := os.Stat(filepath.Join(modelDir, strings.TrimSuffix(rel, "/")))
		if err != nil || fi.IsDir() != isDir {
			return rel
		}
	}
	return ""
}

// Installed 报告 TTS 资产（动态库 + 完整模型）是否齐备。
// 只看文件存在性：已解压文件无法再对归档 sha256 复验，完整性由下载时的校验保证。
func Installed(libDir, ttsDir string, m Model) bool {
	return len(MissingAssets(libDir, ttsDir, m)) == 0
}

// MissingAssets 返回缺失的清单项（顺序：库、模型归档、声码器）。平台无库清单时不计库。
func MissingAssets(libDir, ttsDir string, m Model) []audioassets.Asset {
	var missing []audioassets.Asset
	if lib, err := stt.LibAssetForHost(); err == nil {
		if _, statErr := os.Stat(filepath.Join(libDir, stt.LibName())); statErr != nil {
			missing = append(missing, lib)
		}
	}
	modelDir := ModelDir(ttsDir, m)
	if archiveMissing(modelDir, m) != "" {
		missing = append(missing, archiveAsset(m))
	}
	if m == ModelMatcha {
		if _, err := os.Stat(filepath.Join(modelDir, MatchaVocoderFile)); err != nil {
			missing = append(missing, audioassets.MatchaVocoder())
		}
	}
	return missing
}

// ttsModelMapper 返回模型归档的 mapper：保留归档内完整目录结构，只剥掉顶层目录。
// 为什么不再按文件名白名单挑文件：Melo 需要 dict/、Matcha 需要 espeak-ng-data/、dict/、多份 lexicon 与 fst，
// 白名单漏一项就是"能加载但读错音"这类难排查的缺陷；全量保留最不易错（归档里没有可执行文件）。
func ttsModelMapper(modelDir string, m Model) func(string) (string, bool) {
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
		// Melo 归档里的 model.int8.onnx 只是 133 字节占位文件，不落盘以免被误当成可用模型。
		if m == ModelMelo && rel == "model.int8.onnx" {
			return "", false
		}
		return filepath.Join(modelDir, filepath.FromSlash(rel)), true
	}
}
