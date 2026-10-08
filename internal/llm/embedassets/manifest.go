// Package embedassets 是进程内 ONNX 向量化引擎（EmbeddingGemma / bge-small-zh）外部资产的清单与下载管理器。
//
// 架构依据：ADR-0109 D2 / PROMPT.md §3 P2。
// 硬约束：
// 1. 与语音（Sherpa-ONNX）完全解耦，自带独立钉版本的 libonnxruntime，独立资产清单、独立目录、独立生命周期；
// 2. 严禁 import audioassets / audiorun；
// 3. 资产下载走 internal/downloader，钉死 sha256 校验，损坏或截断自动删除并报错。
package embedassets

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/downloader"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ORTVersion 是进程内向量化绑定的 ONNX Runtime 官方版本。
// 钉死在编译期常量，与 C API 偏移（version 23）对齐。
const ORTVersion = "1.23.2"

// Kind 资产类型。
type Kind string

const (
	KindLib   Kind = "lib"
	KindFile  Kind = "file"
	KindModel Kind = "model"
)

// Asset 描述一个外部资源。
type Asset struct {
	Name   string
	Kind   Kind
	URL    string
	File   string
	SHA256 string
	Size   int64
}

// LibFileName 返回当前操作系统的 ORT 动态库文件名。
func LibFileName(goos string) string {
	switch goos {
	case "darwin":
		return "libonnxruntime.dylib"
	case "windows":
		// 不能叫 onnxruntime.dll：语音的 sherpa c-api.dll 按裸名 "onnxruntime.dll" 导入 ORT，
		// Windows 加载器按基名匹配已加载模块，向量化先加载同名 DLL 会让 sherpa 绑到 1.23.2 而版本不符。
		// 解压时由 mapper 把官方包里的 onnxruntime.dll 落地为此名（ADR-0110 修订三：ORT 冲突排查）。
		return "onnxruntime_embed.dll"
	default:
		return "libonnxruntime.so"
	}
}

// ortLibs 返回受支持平台的 ORT 动态库归档清单。
// 键为 "GOOS/GOARCH"。
func ortLibs() map[string]Asset {
	mk := func(platform, ext, sha string, size int64) Asset {
		return Asset{
			Name:   "onnxruntime " + ORTVersion + " (" + platform + ")",
			Kind:   KindLib,
			URL:    "https://github.com/microsoft/onnxruntime/releases/download/v" + ORTVersion + "/onnxruntime-" + platform + "-" + ORTVersion + "." + ext,
			File:   "onnxruntime-" + platform + "-" + ORTVersion + "." + ext,
			SHA256: sha,
			Size:   size,
		}
	}
	return map[string]Asset{
		"darwin/arm64":  mk("osx-arm64", "tgz", "b4d513ab2b26f088c66891dbbc1408166708773d7cc4163de7bdca0e9bbb7856", 9999931),
		"darwin/amd64":  mk("osx-x86_64", "tgz", "d10359e16347b57d9959f7e80a225a5b4a66ed7d7e007274a15cae86836485a6", 11676322),
		"linux/amd64":   mk("linux-x64", "tgz", "1fa4dcaef22f6f7d5cd81b28c2800414350c10116f5fdd46a2160082551c5f9b", 8309231),
		"linux/arm64":   mk("linux-aarch64", "tgz", "7c63c73560ed76b1fac6cff8204ffe34fe180e70d6582b5332ec094810241e5c", 7254068),
		"windows/amd64": mk("win-x64", "zip", "0b38df9af21834e41e73d602d90db5cb06dbd1ca618948b8f1d66d607ac9f3cd", 78127794),
	}
}

// ORTLibAsset 获取指定平台的 ORT 库资产。
func ORTLibAsset(goos, goarch string) (Asset, bool) {
	a, ok := ortLibs()[goos+"/"+goarch]
	return a, ok
}

// GemmaAssets 返回平衡档 EmbeddingGemma-300M ONNX 资产切片。
func GemmaAssets() []Asset {
	return []Asset{
		{
			Name:   "embeddinggemma-300m-onnx",
			Kind:   KindModel,
			URL:    "https://huggingface.co/onnx-community/embeddinggemma-300m-ONNX/resolve/main/onnx/model_quantized.onnx",
			File:   "model_quantized.onnx",
			SHA256: "172efde319fe1542dc41f31be6154910b05b78f7a861c265c4600eec906bd6d8",
			Size:   567874,
		},
		{
			Name:   "embeddinggemma-300m-onnx-data",
			Kind:   KindModel,
			URL:    "https://huggingface.co/onnx-community/embeddinggemma-300m-ONNX/resolve/main/onnx/model_quantized.onnx_data",
			File:   "model_quantized.onnx_data",
			SHA256: "705626e28e4c23c82ade34566b4197d97f534c12275fa406dfb71e9937d388c0",
			Size:   308890624,
		},
		{
			Name:   "embeddinggemma-300m-tokenizer",
			Kind:   KindFile,
			URL:    "https://huggingface.co/onnx-community/embeddinggemma-300m-ONNX/resolve/main/tokenizer.model",
			File:   "tokenizer.model",
			SHA256: "1299c11d7cf632ef3b4e11937501358ada021bbdf7c47638d13c0ee982f2e79c",
			Size:   4689074,
		},
	}
}

// BGEAssets 返回轻量档 bge-small-zh-v1.5 ONNX 资产切片。
func BGEAssets() []Asset {
	return []Asset{
		{
			Name:   "bge-small-zh-v1.5-onnx",
			Kind:   KindModel,
			URL:    "https://huggingface.co/Xenova/bge-small-zh-v1.5/resolve/main/onnx/model_quantized.onnx",
			File:   "model_quantized.onnx",
			SHA256: "15b717c382bcb518ba457b93ea6850ede7f4f1cd8937454aa06972366cd19bcc",
			Size:   24010842,
		},
		{
			Name:   "bge-small-zh-v1.5-vocab",
			Kind:   KindFile,
			URL:    "https://huggingface.co/Xenova/bge-small-zh-v1.5/resolve/main/vocab.txt",
			File:   "vocab.txt",
			SHA256: "45bbac6b341c319adc98a532532882e91a9cefc0329aa57bac9ae761c27b291c",
			Size:   109540,
		},
	}
}

// EnsureORT 确保指定平台的 ORT 动态库下载并解压到 <embedDir>/ort 目录中。
// 返回解压后的动态库绝对路径。
func EnsureORT(ctx context.Context, client *http.Client, embedDir, goos, goarch string) (string, error) {
	asset, ok := ORTLibAsset(goos, goarch)
	if !ok {
		return "", apperr.New(apperr.CodeUnimplemented, fmt.Sprintf("embedassets: unsupported platform %s/%s", goos, goarch))
	}

	destDir := filepath.Join(embedDir, "ort")
	targetLibPath := filepath.Join(destDir, LibFileName(goos))

	// 如果库文件已存在，直接返回
	if fi, err := os.Stat(targetLibPath); err == nil && fi.Size() > 0 {
		return targetLibPath, nil
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "embedassets: mkdir failed", err)
	}

	opts := downloader.Options{
		SHA256:   asset.SHA256,
		SizeHint: asset.Size,
	}

	mapper := func(name string) (string, bool) {
		base := filepath.Base(name)
		if strings.HasPrefix(base, "libonnxruntime.") && (strings.HasSuffix(base, ".dylib") || strings.Contains(base, ".so")) ||
			base == "onnxruntime.dll" {
			return targetLibPath, true
		}
		return "", false
	}

	slog.Info("embedassets: downloading onnxruntime library", "platform", goos+"/"+goarch, "url", asset.URL)

	if strings.HasSuffix(asset.File, ".zip") {
		if err := downloader.DownloadExtractZipOpts(ctx, client, asset.URL, destDir, mapper, opts); err != nil {
			return "", apperr.Wrap(apperr.CodeInternal, "embedassets: download zip failed", err)
		}
	} else {
		if err := downloader.DownloadExtractTarGzOpts(ctx, client, asset.URL, destDir, mapper, opts); err != nil {
			return "", apperr.Wrap(apperr.CodeInternal, "embedassets: download tar.gz failed", err)
		}
	}

	if fi, err := os.Stat(targetLibPath); err != nil || fi.Size() == 0 {
		return "", apperr.New(apperr.CodeInternal, "embedassets: target library file missing after extraction: "+targetLibPath)
	}

	return targetLibPath, nil
}

// EnsureGemma 确保 EmbeddingGemma-300M 资产就绪。
// 返回 modelPath, tokPath。
func EnsureGemma(ctx context.Context, client *http.Client, embedDir string) (string, string, error) {
	destDir := filepath.Join(embedDir, "embeddinggemma")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", "", apperr.Wrap(apperr.CodeInternal, "embedassets: mkdir failed", err)
	}

	for _, a := range GemmaAssets() {
		targetPath := filepath.Join(destDir, a.File)
		opts := downloader.Options{
			SHA256:   a.SHA256,
			SizeHint: a.Size,
		}
		if err := downloader.DownloadFileOpts(ctx, client, a.URL, targetPath, opts); err != nil {
			return "", "", apperr.Wrap(apperr.CodeInternal, "embedassets: download gemma asset failed: "+a.File, err)
		}
	}

	modelPath := filepath.Join(destDir, "model_quantized.onnx")
	tokPath := filepath.Join(destDir, "tokenizer.model")
	return modelPath, tokPath, nil
}

// EnsureBGE 确保 bge-small-zh-v1.5 资产就绪。
// 返回 modelPath, vocabPath。
func EnsureBGE(ctx context.Context, client *http.Client, embedDir string) (string, string, error) {
	destDir := filepath.Join(embedDir, "bge-small-zh")
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", "", apperr.Wrap(apperr.CodeInternal, "embedassets: mkdir failed", err)
	}

	for _, a := range BGEAssets() {
		targetPath := filepath.Join(destDir, a.File)
		opts := downloader.Options{
			SHA256:   a.SHA256,
			SizeHint: a.Size,
		}
		if err := downloader.DownloadFileOpts(ctx, client, a.URL, targetPath, opts); err != nil {
			return "", "", apperr.Wrap(apperr.CodeInternal, "embedassets: download bge asset failed: "+a.File, err)
		}
	}

	modelPath := filepath.Join(destDir, "model_quantized.onnx")
	vocabPath := filepath.Join(destDir, "vocab.txt")
	return modelPath, vocabPath, nil
}
