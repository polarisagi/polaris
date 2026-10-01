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
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SherpaABIVersion 是本包与 tts 包手写 FFI 结构体偏移所对应的 sherpa-onnx 版本。
// 偏移按此版本 c-api.h 经 clang offsetof 实测（识别器配置 608B、结果结构 json=40/lang=48/
// emotion=56/event=64、标点配置 24B、Kokoro TTS 配置 448B）。
// 为什么钉死在编译期常量：偏移与库版本强耦合，用户改 sherpa_version 即可让偏移失配并
// 造成内存破坏，而该风险在配置层不可见。升级版本必须重测全部偏移后同步改本常量。
const SherpaABIVersion = "1.13.2"

// sherpaReleaseBase 是 sherpa-onnx 预编译库的 GitHub Releases 前缀（版本占位 %s）。
const sherpaReleaseBase = "https://github.com/k2-fsa/sherpa-onnx/releases/download/v%s/"

// libPlatformSuffix 是 GOOS/GOARCH → 资产名后缀（位于 "sherpa-onnx-v{V}-" 与 ".tar.bz2" 之间）。
// 统一选用 -lib 变体（只含动态库，体积约为 -shared 的一半）。
// 资产名是对 GitHub API 资产清单实测的结果；曾因手写平台名（osx-x86_64 / linux-x86_64 /
// win-x64-shared 均不存在）导致 6 个平台里 5 个 404，由 make audio-nettest 做 HEAD 校验防再犯。
// windows 取 MT（静态 CRT）变体，免装 VC++ 运行库。
// 用函数而非包级 map：internal/ 禁全局可变变量（map 可被任意修改）。
func libPlatformSuffix(platform string) (string, bool) {
	switch platform {
	case "darwin/arm64":
		return "osx-arm64-shared-lib", true
	case "darwin/amd64":
		return "osx-x64-shared-lib", true
	case "linux/amd64":
		return "linux-x64-shared-lib", true
	case "linux/arm64":
		return "linux-aarch64-shared-cpu-lib", true
	case "windows/amd64":
		return "win-x64-shared-MT-Release-lib", true
	}
	return "", false
}

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

// SherpaLibURL 返回当前 OS/ARCH 对应的 sherpa-onnx 预编译库下载地址（GitHub Releases）。
// 导出供 tts 包复用，避免重复维护平台映射表。
func SherpaLibURL(version string) (string, error) {
	return libDownloadURL(runtime.GOOS, runtime.GOARCH, version)
}

// SherpaLibURLFor 返回指定平台的下载地址（供契约测试遍历全部平台）。
func SherpaLibURLFor(goos, goarch, version string) (string, error) {
	return libDownloadURL(goos, goarch, version)
}

// SherpaLibPlatforms 返回受支持的 "GOOS/GOARCH" 列表（供契约测试遍历）。
func SherpaLibPlatforms() []string {
	return []string{"darwin/arm64", "darwin/amd64", "linux/amd64", "linux/arm64", "windows/amd64"}
}

func libDownloadURL(goos, goarch, version string) (string, error) {
	if version == "" {
		return "", apperr.New(apperr.CodeInternal, "stt: sherpa version is empty")
	}
	suffix, ok := libPlatformSuffix(goos + "/" + goarch)
	if !ok {
		return "", apperr.New(apperr.CodeInternal, "unsupported platform: "+goos+"/"+goarch)
	}
	return fmt.Sprintf(sherpaReleaseBase+"sherpa-onnx-v%s-%s.tar.bz2", version, version, suffix), nil
}

// EnsureAssets 确保 sttDir 下存在可用的动态库与模型文件，幂等。
// 中国大陆网络下自动通过 ghproxy 加速下载。onStep 非 nil 时在每个耗时步骤前回调，
// 供调用方把"当前在做什么"暴露给状态机（nil 安全）。
func EnsureAssets(ctx context.Context, sttDir string, httpClient *http.Client, version, modelURL, punctModelURL string, onStep func(string)) error {
	step := func(s string) {
		if onStep != nil {
			onStep(s)
		}
	}
	ver, err := ResolveSherpaVersion(version)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(sttDir, 0o755); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "stt: mkdir "+sttDir+" failed", err)
	}

	// ── 1. sherpa-onnx 动态库 ─────────────────────────────────────────────────
	libPath := filepath.Join(sttDir, LibName())
	if _, err := os.Stat(libPath); os.IsNotExist(err) {
		rawURL, err := libDownloadURL(runtime.GOOS, runtime.GOARCH, ver)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "stt: download sherpa-onnx failed", err)
		}
		step("下载 sherpa 库")
		slog.Info("stt: downloading sherpa-onnx library", "dest", libPath, "url", rawURL)
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
		step("下载 SenseVoice 模型 " + archiveBaseName(modelURL))
		slog.Info("stt: downloading SenseVoice model", "dest", modelDir, "url", modelURL)
		if err := downloader.DownloadExtractTarBz2(ctx, httpClient, modelURL, modelDir, sttModelMapper(modelDir)); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "stt: model download failed", err)
		}
		// 解压后必须校验必需文件：归档内文件名与 mapper 不匹配时 extractTar 仍会因写出
		// 了其他文件而判成功，导致每次启动重下 + 引擎创建失败的死循环（S3）。
		if err := requireFiles(modelDir, modelURL, "model.onnx", "tokens.txt"); err != nil {
			return err
		}
		slog.Info("stt: model ready", "dir", modelDir)
	} else {
		slog.Info("stt: model already present, skipping download", "dir", modelDir)
	}

	// ── 3. 标点模型文件 ───────────────────────────────────────────────────────
	if punctModelURL != "" {
		return ensurePunctModel(ctx, filepath.Join(sttDir, "punct_model"), httpClient, punctModelURL, step)
	}
	return nil
}

// ensurePunctModel 幂等地准备标点模型（model.onnx 已存在则跳过）。
func ensurePunctModel(ctx context.Context, punctDir string, httpClient *http.Client, punctModelURL string, step func(string)) error {
	if _, err := os.Stat(filepath.Join(punctDir, "model.onnx")); err == nil {
		slog.Info("stt: punctuation model already present, skipping download", "dir", punctDir)
		return nil
	}
	step("下载标点模型 " + archiveBaseName(punctModelURL))
	slog.Info("stt: downloading punctuation model", "dest", punctDir, "url", punctModelURL)
	if err := downloader.DownloadExtractTarBz2(ctx, httpClient, punctModelURL, punctDir, punctModelMapper(punctDir)); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "stt: punctuation model download failed", err)
	}
	if err := requireFiles(punctDir, punctModelURL, "model.onnx"); err != nil {
		return err
	}
	slog.Info("stt: punctuation model ready", "dir", punctDir)
	return nil
}

// requireFiles 校验 dir 下 names 全部存在，缺失时返回带归档名的明确错误。
func requireFiles(dir, archiveURL string, names ...string) error {
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			return apperr.New(apperr.CodeInternal, fmt.Sprintf(
				"stt: 归档 %s 解压后缺少必需文件 %s（目录 %s）；归档内文件名可能与 mapper 不匹配",
				archiveBaseName(archiveURL), n, dir))
		}
	}
	return nil
}

// archiveBaseName 取 URL 的最后一段（归档名），用于错误信息与步骤描述。
func archiveBaseName(u string) string {
	return filepath.Base(u)
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
