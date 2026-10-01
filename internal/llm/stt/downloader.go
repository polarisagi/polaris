package stt

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"

	"github.com/polarisagi/polaris/internal/downloader"
	"github.com/polarisagi/polaris/internal/llm/audioassets"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SherpaABIVersion 是本包与 tts 包手写 FFI 结构体偏移所对应的 sherpa-onnx 版本，
// 定义与说明见 audioassets.SherpaABIVersion（清单 sha256 与该版本绑定，故常量归清单包持有）。
const SherpaABIVersion = audioassets.SherpaABIVersion

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

// ResolveSherpaVersion 校验配置的 sherpa 版本并返回实际使用的版本。
// 空串 → SherpaABIVersion；与 SherpaABIVersion 不同 → 明确报错（不下载、不加载）。
// stt 与 tts 的 EnsureAssets 共用，保证两条路径对 ABI 钉死的处理一致。
func ResolveSherpaVersion(configured string) (string, error) {
	if configured == "" || configured == SherpaABIVersion {
		return SherpaABIVersion, nil
	}
	return "", apperr.New(apperr.CodeInvalidInput, fmt.Sprintf(
		"stt: sherpa_version=%q 与代码内 FFI 结构体偏移所对应的 ABI 版本 %q 不一致；"+
			"偏移按 %s 的 c-api.h 实测手写，换版本会导致内存破坏。请清空 sherpa_version 或设为 %s",
		configured, SherpaABIVersion, SherpaABIVersion, SherpaABIVersion))
}

// LibAssetForHost 返回当前 OS/ARCH 的动态库清单项；平台不受支持时报错。
func LibAssetForHost() (audioassets.Asset, error) {
	a, ok := audioassets.LibAsset(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return audioassets.Asset{}, apperr.New(apperr.CodeUnimplemented,
			"stt: 不支持的平台 "+runtime.GOOS+"/"+runtime.GOARCH+"（无对应 sherpa-onnx 预编译库）")
	}
	return a, nil
}

// EnsureLib 确保 libDir 下存在 sherpa-onnx 动态库，幂等；stt 与 tts 共用同一份库。
// 下载经清单 sha256 校验，进度经 progress 汇报（nil 安全）。
func EnsureLib(ctx context.Context, libDir string, httpClient *http.Client, progress audioassets.ProgressFunc) error {
	libPath := filepath.Join(libDir, LibName())
	if _, err := os.Stat(libPath); err == nil {
		return nil
	}
	asset, err := LibAssetForHost()
	if err != nil {
		return err
	}
	slog.Info("audio: downloading sherpa-onnx library", "dest", libPath, "asset", asset.File, "bytes", asset.Size)
	if err := downloader.DownloadExtractLibsOpts(ctx, httpClient, asset.URL(), libDir, assetOptions(asset, progress)); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "stt: library download failed", err)
	}
	if _, err := os.Stat(libPath); err != nil {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf(
			"stt: 归档 %s 解压后缺少 %s（目录 %s）", asset.File, LibName(), libDir))
	}
	slog.Info("audio: sherpa-onnx library ready", "path", libPath)
	return nil
}

// assetOptions 把清单项转成下载选项（sha256 + 字节数提示 + 进度）。
func assetOptions(a audioassets.Asset, progress audioassets.ProgressFunc) downloader.Options {
	o := downloader.Options{SHA256: a.SHA256, SizeHint: a.Size}
	if progress != nil {
		o.Progress = func(done, total int64) { progress(a, done, total) }
	}
	return o
}

// AssetOptions 供 tts 包复用同一套"清单项 → 下载选项"转换。
func AssetOptions(a audioassets.Asset, progress audioassets.ProgressFunc) downloader.Options {
	return assetOptions(a, progress)
}

// EnsureAssets 确保 sttDir 下存在可用的动态库与模型文件（SenseVoice int8 + 标点），幂等。
// 中国大陆网络下自动通过 ghproxy 加速下载。全部下载经清单 sha256 校验。
func EnsureAssets(ctx context.Context, sttDir string, httpClient *http.Client, version string, progress audioassets.ProgressFunc) error {
	if _, err := ResolveSherpaVersion(version); err != nil {
		return err
	}
	if err := os.MkdirAll(sttDir, 0o755); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "stt: mkdir "+sttDir+" failed", err)
	}

	// ── 1. sherpa-onnx 动态库 ─────────────────────────────────────────────────
	if err := EnsureLib(ctx, sttDir, httpClient, progress); err != nil {
		return err
	}

	// ── 2. SenseVoice 模型文件 ───────────────────────────────────────────────
	modelAsset := audioassets.STTModel()
	modelDir := ModelDir(sttDir)
	if !modelFilesPresent(modelDir) {
		slog.Info("stt: downloading SenseVoice model", "dest", modelDir, "asset", modelAsset.File, "bytes", modelAsset.Size)
		if err := downloader.DownloadExtractTarBz2Opts(ctx, httpClient, modelAsset.URL(), modelDir,
			sttModelMapper(modelDir), assetOptions(modelAsset, progress)); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "stt: model download failed", err)
		}
		// 解压后必须校验必需文件：归档内文件名与 mapper 不匹配时 extractTar 仍会因写出
		// 了其他文件而判成功，导致每次重下 + 引擎创建失败的死循环（S3）。
		if err := requireFiles(modelDir, modelAsset.File, "model.onnx", "tokens.txt"); err != nil {
			return err
		}
		slog.Info("stt: model ready", "dir", modelDir)
	}

	// ── 3. 标点模型文件 ───────────────────────────────────────────────────────
	return ensurePunctModel(ctx, PunctModelDir(sttDir), httpClient, progress)
}

// ensurePunctModel 幂等地准备标点模型（model.onnx 已存在则跳过）。
func ensurePunctModel(ctx context.Context, punctDir string, httpClient *http.Client, progress audioassets.ProgressFunc) error {
	if _, err := os.Stat(filepath.Join(punctDir, "model.onnx")); err == nil {
		return nil
	}
	asset := audioassets.PunctModel()
	slog.Info("stt: downloading punctuation model", "dest", punctDir, "asset", asset.File, "bytes", asset.Size)
	if err := downloader.DownloadExtractTarBz2Opts(ctx, httpClient, asset.URL(), punctDir,
		punctModelMapper(punctDir), assetOptions(asset, progress)); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "stt: punctuation model download failed", err)
	}
	if err := requireFiles(punctDir, asset.File, "model.onnx"); err != nil {
		return err
	}
	slog.Info("stt: punctuation model ready", "dir", punctDir)
	return nil
}

// Installed 报告 sttDir 下 STT 资产（库 + 模型 + 标点）是否齐备。
// 只看文件存在性：已解压的模型文件无法再对归档 sha256 复验，完整性由下载时的校验保证。
func Installed(sttDir string) bool {
	return len(MissingAssets(sttDir)) == 0
}

// MissingAssets 返回 sttDir 下缺失的清单项（顺序：库、模型、标点）。
// 平台不受支持（无库清单）时返回空切片——那是 unsupported 而非"缺资产"，由调用方先判平台。
func MissingAssets(sttDir string) []audioassets.Asset {
	var missing []audioassets.Asset
	if lib, err := LibAssetForHost(); err == nil {
		if _, statErr := os.Stat(filepath.Join(sttDir, LibName())); statErr != nil {
			missing = append(missing, lib)
		}
	}
	if !modelFilesPresent(ModelDir(sttDir)) {
		missing = append(missing, audioassets.STTModel())
	}
	if _, err := os.Stat(filepath.Join(PunctModelDir(sttDir), "model.onnx")); err != nil {
		missing = append(missing, audioassets.PunctModel())
	}
	return missing
}

// requireFiles 校验 dir 下 names 全部存在，缺失时返回带归档名的明确错误。
func requireFiles(dir, archiveName string, names ...string) error {
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			return apperr.New(apperr.CodeInternal, fmt.Sprintf(
				"stt: 归档 %s 解压后缺少必需文件 %s（目录 %s）；归档内文件名可能与 mapper 不匹配",
				archiveName, n, dir))
		}
	}
	return nil
}

// sttModelMapper 返回 SenseVoice 归档的 mapper：
// model.onnx / model.int8.onnx → model.onnx（int8 归档内文件名带 .int8，引擎只认 model.onnx）；
// tokens.txt 原样。
func sttModelMapper(modelDir string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		switch filepath.Base(name) {
		case "model.onnx", "model.int8.onnx":
			return filepath.Join(modelDir, "model.onnx"), true
		case "tokens.txt":
			return filepath.Join(modelDir, "tokens.txt"), true
		}
		return "", false
	}
}

// punctModelMapper 返回标点模型归档的 mapper：
// model.onnx / model.int8.onnx → model.onnx；tokens.json 保留（C 配置只需模型路径，
// 但文件随模型分发，保留便于排障）。与 STT mapper 刻意分开：两者必需文件集不同。
func punctModelMapper(punctDir string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		switch filepath.Base(name) {
		case "model.onnx", "model.int8.onnx":
			return filepath.Join(punctDir, "model.onnx"), true
		case "tokens.json":
			return filepath.Join(punctDir, "tokens.json"), true
		}
		return "", false
	}
}

// modelFilesPresent 检查 STT 模型目录下必要文件是否已存在。
func modelFilesPresent(modelDir string) bool {
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
