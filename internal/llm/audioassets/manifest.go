// Package audioassets 是语音（STT/TTS）外部资产的唯一清单：URL、sha256、字节数、归档类别。
//
// 为什么集中成一份清单：此前 URL 散落在 defaults.toml、stt/tts 两个下载器与 nettest 里，
// 6 个平台 5 个 URL 404 而单测全绿（ADR-0106）；且下载后无任何完整性校验。清单让
// "下载 → sha256 校验 → nettest 逐项 HEAD 比对 Content-Length" 共用同一份事实（ADR-0107）。
// 不开放为配置项：URL 可配置就意味着 sha256 无法钉死，校验形同虚设。
package audioassets

import (
	"fmt"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// SherpaABIVersion 是 stt/tts 两包手写 FFI 结构体偏移所对应的 sherpa-onnx 版本。
// 偏移按此版本 c-api.h 经 clang offsetof 实测（识别器配置 608B、结果结构 json=40/lang=48/
// emotion=56/event=64、标点配置 24B、Kokoro TTS 配置 448B）。
// 为什么钉死在编译期常量：偏移与库版本强耦合，用户改 sherpa_version 即可让偏移失配并
// 造成内存破坏，而该风险在配置层不可见。升级版本必须重测全部偏移、重算清单 sha256 后
// 同步改本常量。
const SherpaABIVersion = "1.13.2"

// releaseBase 是 sherpa-onnx 在 GitHub Releases 的下载前缀；各资产的 Tag 拼在其后。
const releaseBase = "https://github.com/k2-fsa/sherpa-onnx/releases/download/"

// Kind 区分资产的解压方式。
type Kind string

const (
	// KindLib 只含动态库（.so/.dylib/.dll），按文件名平铺解到库目录。
	KindLib Kind = "lib"
	// KindSTTModel SenseVoice 模型：只取 model(.int8).onnx 与 tokens.txt。
	KindSTTModel Kind = "stt_model"
	// KindPunctModel 标点模型：只取 model(.int8).onnx 与 tokens.json。
	KindPunctModel Kind = "punct_model"
	// KindTTSModel Kokoro 模型：保留归档内完整目录结构（剥掉顶层目录）。
	KindTTSModel Kind = "tts_model"
)

// ProgressFunc 汇报某个资产的下载进度。total 为 0 表示总量未知。
// 回调同步执行在下载 goroutine 中，必须快速返回。
type ProgressFunc func(a Asset, done, total int64)

// Asset 描述一个可下载归档。
type Asset struct {
	Name   string // 展示名（进度文案 / 日志）
	Kind   Kind
	Tag    string // GitHub Release tag（"v1.13.2" / "asr-models" / ...）
	File   string // 归档文件名
	SHA256 string // 小写十六进制
	Size   int64  // 字节数（GitHub API size 字段）
}

// URL 返回直连下载地址（镜像降级由 downloader 层处理）。
func (a Asset) URL() string { return releaseBase + a.Tag + "/" + a.File }

// libs 返回全部平台的 sherpa 动态库资产，键为 "GOOS/GOARCH"。
// 用函数而非包级 map：internal/ 禁全局可变变量（map 可被任意修改）。
// 统一选 -lib 变体（只含动态库，体积约为 -shared 的一半）；windows 取 MT（静态 CRT）变体，
// 免装 VC++ 运行库；linux/arm64 的资产名带 "cpu"。资产名对 GitHub API 实测，非手写猜测。
func libs() map[string]Asset {
	v := "v" + SherpaABIVersion
	mk := func(suffix, sha string, size int64) Asset {
		return Asset{
			Name: "sherpa-onnx 库 (" + suffix + ")", Kind: KindLib, Tag: v,
			File:   "sherpa-onnx-" + v + "-" + suffix + ".tar.bz2",
			SHA256: sha, Size: size,
		}
	}
	return map[string]Asset{
		"darwin/amd64":  mk("osx-x64-shared-lib", "4f94d99687eabb7e92f4c547bd4b26b4ddef9702cf8405c7d7e22c49a4e8bb49", 16759428),
		"darwin/arm64":  mk("osx-arm64-shared-lib", "9e536cb025bd4cefcf6401d24cd1810445143f6c4af38116d91335190695a205", 14810545),
		"linux/amd64":   mk("linux-x64-shared-lib", "bb2da15d4ab5fea369b91edf3caf98bc25f95b3bbaf856215c87b20f69f347ef", 9061670),
		"linux/arm64":   mk("linux-aarch64-shared-cpu-lib", "44449a83f19649b2466c97b4d2df57c158dbede6e1e044f6519cf297df17585c", 11688458),
		"windows/amd64": mk("win-x64-shared-MT-Release-lib", "de56e29a406653c0098770ac76cd2837413e95bc546ffad7f047968c360a2f3f", 7466796),
	}
}

// LibPlatforms 返回受支持的 "GOOS/GOARCH" 列表（顺序稳定，供遍历与契约测试）。
func LibPlatforms() []string {
	return []string{"darwin/arm64", "darwin/amd64", "linux/amd64", "linux/arm64", "windows/amd64"}
}

// LibAsset 返回指定平台的动态库资产；平台不受支持时 ok=false。
func LibAsset(goos, goarch string) (Asset, bool) {
	a, ok := libs()[goos+"/"+goarch]
	return a, ok
}

// STTModel 返回 SenseVoice int8 归档（实测 RTF 0.05@2 线程，RSS≈420MB）。
// 全档统一 int8：fp32 归档 886MB 且无可测精度收益。
func STTModel() Asset {
	return Asset{
		Name: "SenseVoice int8 语音识别模型", Kind: KindSTTModel, Tag: "asr-models",
		File:   "sherpa-onnx-sense-voice-zh-en-ja-ko-yue-int8-2025-09-09.tar.bz2",
		SHA256: "7305f7905bfcf77fa0b39388a313f3da35c68d971661a65475b56fb2162c8e63",
		Size:   165783878,
	}
}

// PunctModel 返回标点模型 int8 归档。
func PunctModel() Asset {
	return Asset{
		Name: "标点模型 int8", Kind: KindPunctModel, Tag: "punctuation-models",
		File:   "sherpa-onnx-punct-ct-transformer-zh-en-vocab272727-2024-04-12-int8.tar.bz2",
		SHA256: "c0d5aa5f8eeb686032345e180bedf39319dc2e0556781c6264bcadba8328a6e1",
		Size:   64717756,
	}
}

// KokoroModel 返回 Kokoro v1.1 fp32 归档。
// 只保留 fp32：int8 在无 VNNI 的 x86 上 RTF 1.4–1.8（慢于实时），fp32 全平台单一资产、无 ISA 分支。
func KokoroModel() Asset {
	return Asset{
		Name: "Kokoro v1.1 语音合成模型", Kind: KindTTSModel, Tag: "tts-models",
		File:   "kokoro-multi-lang-v1_1.tar.bz2",
		SHA256: "a3f4c73d043860e3fd2e5b06f36795eb81de0fc8e8de6df703245edddd87dbad",
		Size:   364816464,
	}
}

// All 返回清单全部资产（5 个平台库 + 3 个模型），供 nettest 与完整性测试遍历。
func All() []Asset {
	out := make([]Asset, 0, 8)
	m := libs()
	for _, p := range LibPlatforms() {
		out = append(out, m[p])
	}
	return append(out, STTModel(), PunctModel(), KokoroModel())
}

// Validate 校验清单自身的格式（sha256 为 64 位小写十六进制、字节数为正、文件名非空）。
// 清单是手工录入的实测值，格式错误会让下载永远校验失败，宁可在单测里提前红。
func (a Asset) Validate() error {
	if a.File == "" || a.Tag == "" || a.Size <= 0 {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("audioassets: 资产 %q 缺少 file/tag/size", a.Name))
	}
	if len(a.SHA256) != 64 {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("audioassets: 资产 %q 的 sha256 长度 %d != 64", a.Name, len(a.SHA256)))
	}
	for _, c := range a.SHA256 {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("audioassets: 资产 %q 的 sha256 含非小写十六进制字符 %q", a.Name, c))
		}
	}
	return nil
}
