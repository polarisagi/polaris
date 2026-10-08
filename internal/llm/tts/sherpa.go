package tts

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"

	"github.com/polarisagi/polaris/internal/llm/stt"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ttsFuncs 持有 sherpa-onnx TTS 相关的 purego 函数指针。
type ttsFuncs struct {
	CreateOfflineTts                func(config uintptr) uintptr
	DestroyOfflineTts               func(tts uintptr)
	OfflineTtsGenerate              func(tts uintptr, text uintptr, sid int32, speed float32) uintptr
	DestroyOfflineTtsGeneratedAudio func(audio uintptr)
}

// Library 封装 sherpa-onnx TTS 函数指针，避免包级导出可变变量（CLAUDE.md「internal/
// 禁全局可变变量」；与 internal/llm/stt/sherpa.go 的既有 Library 模式对齐）。
type Library struct {
	funcs ttsFuncs
}

var (
	libMu   sync.Mutex
	loadErr error
	libInst *Library
)

// LoadLibrary 延迟加载 sherpa-onnx 动态库并映射 TTS 符号。
// 幂等可重入：已加载则直接返回 nil；加载失败后可再次尝试（下载完成后调用）。
func LoadLibrary(libPath string) error {
	libMu.Lock()
	defer libMu.Unlock()

	if libInst != nil {
		return nil // 已成功加载，直接复用
	}

	lib, err := stt.Dlopen(libPath)
	if err != nil {
		loadErr = err
		return loadErr
	}

	var tf ttsFuncs
	purego.RegisterLibFunc(&tf.CreateOfflineTts, lib, "SherpaOnnxCreateOfflineTts")
	purego.RegisterLibFunc(&tf.DestroyOfflineTts, lib, "SherpaOnnxDestroyOfflineTts")
	purego.RegisterLibFunc(&tf.OfflineTtsGenerate, lib, "SherpaOnnxOfflineTtsGenerate")
	purego.RegisterLibFunc(&tf.DestroyOfflineTtsGeneratedAudio, lib, "SherpaOnnxDestroyOfflineTtsGeneratedAudio")

	libInst = &Library{funcs: tf}
	loadErr = nil
	return nil
}

// Options 是 TTS 引擎的运行参数。
type Options struct {
	// Model 选择 Melo 或 Matcha；必填。
	Model Model
	// NumThreads 推理线程数；<=0 取 2。
	NumThreads int
	// Speed 语速倍率；<=0 取 1.0。
	Speed float32
}

// Engine 是 Sherpa-ONNX 本地 TTS 引擎（MeloTTS / Matcha），实现 Provider 接口。
// 两个模型都是单说话人，sid 恒为 0（ADR-0110 决策 1）。
type Engine struct {
	mu    sync.Mutex
	tts   uintptr
	lib   *Library
	model Model
	speed float32
}

// TTS 配置结构体布局（SherpaOnnxOfflineTtsConfig，v1.13.2，总长 448B，arm64 实测）。
//
// 测量方法（升级 sherpa 版本必须重测）：下载 tag v1.13.2 的 sherpa-onnx/c-api/c-api.h，
// 写临时 C 文件 `#include "c-api.h"` 后对每个字段 printf offsetof(SherpaOnnxOfflineTtsConfig, ...)，
// 用 clang 编译运行。实测结果（字节偏移）：
//
//	sizeof(SherpaOnnxOfflineTtsConfig)=448, sizeof(SherpaOnnxOfflineTtsModelConfig)=416
//	model.vits:    model=0 lexicon=8 tokens=16 data_dir=24 noise_scale=32 noise_scale_w=36
//	               length_scale=40 dict_dir=48
//	model.num_threads=56 model.debug=60 model.provider=64
//	model.matcha:  acoustic_model=72 vocoder=80 lexicon=88 tokens=96 data_dir=104
//	               noise_scale=112 length_scale=116 dict_dir=120
//	(model.kokoro 自 128 起，本包不再使用)
//	rule_fsts=416 max_num_sentences=424 rule_fars=432 silence_scale=440
//
// 注意：1.13.2 的 c-api.cc 创建 TTS 时不拷贝 vits/matcha 的 dict_dir（头文件注明为遗留字段），
// MeloTTS 的 jieba 词典由库内置，所以 dict_dir 写了也不生效；这里仍按偏移写入以对齐
// 上游 Python 参考配置，不依赖它。noise 参数显式写为模型默认值
// （vits 0.667/0.8/1.0，matcha 1.0/1.0）：c-api.cc 对 matcha noise_scale 的兜底是 0.667，
// 与用户试听所用的 Python 默认 1.0 不同，所以不能留 0 让 c-api 兜底。
const (
	ttsConfigSize         = 448
	offsetModelNumThreads = 56
	offsetModelDebug      = 60
	offsetModelProvider   = 64
	offsetRuleFsts        = 416
	offsetMaxNumSentences = 424
	offsetRuleFars        = 432
	offsetSilenceScale    = 440

	// model.vits（Melo）
	offsetVitsModel       = 0
	offsetVitsLexicon     = 8
	offsetVitsTokens      = 16
	offsetVitsDataDir     = 24
	offsetVitsNoiseScale  = 32
	offsetVitsNoiseScaleW = 36
	offsetVitsLengthScale = 40
	offsetVitsDictDir     = 48

	// model.matcha
	offsetMatchaAcoustic    = 72
	offsetMatchaVocoder     = 80
	offsetMatchaLexicon     = 88
	offsetMatchaTokens      = 96
	offsetMatchaDataDir     = 104
	offsetMatchaNoiseScale  = 112
	offsetMatchaLengthScale = 116
	offsetMatchaDictDir     = 120
)

// maxNumSentences 必须是 100：sherpa 按标点（含逗号）切子句，=1 时每个子句单独合成，
// 短子句会被吞字（"我说另外，请打开。"→"你拿开"）。整句同批送入才正确（ADR-0110 决策 6）。
// 长文本的内存峰值由 Generate 里的自行切句控制。
const maxNumSentences = 100

// interSentenceSilence 是自行切句后各句之间补的静音时长。sherpa 的 silence_scale 只作用于
// 它内部的多句批处理，我们逐句调用时句间会"硬接"，补一小段让节奏自然。
const interSentenceSilence = 0.12

// NewEngine 构造新的 Sherpa-ONNX 离线 TTS 引擎。
// 库未加载或必需文件缺失时返回错误，不构造"空壳引擎"。
func NewEngine(modelDir string, opts Options) (*Engine, error) {
	libMu.Lock()
	lib := libInst
	libMu.Unlock()

	if lib == nil {
		return nil, apperr.New(apperr.CodeInternal, "tts: library not loaded")
	}
	if _, err := ParseModel(string(opts.Model)); err != nil {
		return nil, err
	}
	if miss := ModelMissing(modelDir, opts.Model); miss != "" {
		return nil, apperr.New(apperr.CodeInternal, "tts: 模型目录 "+modelDir+" 缺少必需文件 "+miss)
	}
	if opts.NumThreads <= 0 {
		opts.NumThreads = 2
	}
	if opts.Speed <= 0 {
		opts.Speed = 1.0
	}

	configData := make([]byte, ttsConfigSize)
	cfgPtr := uintptr(unsafe.Pointer(&configData[0]))

	var refs [][]byte
	cString := func(s string) uintptr {
		if s == "" {
			return 0
		}
		b := append([]byte(s), 0)
		refs = append(refs, b)
		return uintptr(unsafe.Pointer(&b[0]))
	}
	defer runtime.KeepAlive(refs)
	defer runtime.KeepAlive(configData)

	var fsts []string
	for _, f := range ruleFstFiles(opts.Model) {
		p := filepath.Join(modelDir, f)
		if _, err := os.Stat(p); err != nil {
			// 不是致命错误（引擎仍可合成），但数字/日期读法会退化，必须留痕而非静默。
			slog.Warn("tts: 规则 FST 缺失，数字/日期读法可能不正确", "file", p, "err", err)
			continue
		}
		fsts = append(fsts, p)
	}

	*(*int32)(unsafe.Pointer(cfgPtr + offsetModelNumThreads)) = int32(opts.NumThreads)
	*(*int32)(unsafe.Pointer(cfgPtr + offsetModelDebug)) = 0
	*(*uintptr)(unsafe.Pointer(cfgPtr + offsetModelProvider)) = cString("cpu")

	join := func(f string) string { return filepath.Join(modelDir, f) }
	switch opts.Model {
	case ModelMelo:
		*(*uintptr)(unsafe.Pointer(cfgPtr + offsetVitsModel)) = cString(join("model.onnx"))
		*(*uintptr)(unsafe.Pointer(cfgPtr + offsetVitsLexicon)) = cString(join("lexicon.txt"))
		*(*uintptr)(unsafe.Pointer(cfgPtr + offsetVitsTokens)) = cString(join("tokens.txt"))
		*(*uintptr)(unsafe.Pointer(cfgPtr + offsetVitsDictDir)) = cString(join("dict"))
		*(*float32)(unsafe.Pointer(cfgPtr + offsetVitsNoiseScale)) = 0.667
		*(*float32)(unsafe.Pointer(cfgPtr + offsetVitsNoiseScaleW)) = 0.8
		*(*float32)(unsafe.Pointer(cfgPtr + offsetVitsLengthScale)) = 1.0
	case ModelMatcha:
		*(*uintptr)(unsafe.Pointer(cfgPtr + offsetMatchaAcoustic)) = cString(join("model-steps-3.onnx"))
		*(*uintptr)(unsafe.Pointer(cfgPtr + offsetMatchaVocoder)) = cString(join(MatchaVocoderFile))
		*(*uintptr)(unsafe.Pointer(cfgPtr + offsetMatchaLexicon)) = cString(join("lexicon.txt"))
		*(*uintptr)(unsafe.Pointer(cfgPtr + offsetMatchaTokens)) = cString(join("tokens.txt"))
		*(*uintptr)(unsafe.Pointer(cfgPtr + offsetMatchaDataDir)) = cString(join("espeak-ng-data"))
		*(*float32)(unsafe.Pointer(cfgPtr + offsetMatchaNoiseScale)) = 1.0
		*(*float32)(unsafe.Pointer(cfgPtr + offsetMatchaLengthScale)) = 1.0
	}

	*(*uintptr)(unsafe.Pointer(cfgPtr + offsetRuleFsts)) = cString(strings.Join(fsts, ","))
	*(*int32)(unsafe.Pointer(cfgPtr + offsetMaxNumSentences)) = maxNumSentences
	*(*uintptr)(unsafe.Pointer(cfgPtr + offsetRuleFars)) = 0
	*(*float32)(unsafe.Pointer(cfgPtr + offsetSilenceScale)) = 0.2

	tts := lib.funcs.CreateOfflineTts(cfgPtr)
	if tts == 0 {
		return nil, apperr.New(apperr.CodeInternal, "tts: failed to create offline tts engine")
	}

	return &Engine{tts: tts, lib: lib, model: opts.Model, speed: opts.Speed}, nil
}

// Model 返回引擎所用模型。
func (e *Engine) Model() Model { return e.model }

// Generate 实现 Provider 接口：按句末标点切句，逐句合成后拼接 PCM，输出单个 WAV。
// ctx 仅在句与句之间检查（sherpa 单句推理是同步 FFI，无法中断）。
func (e *Engine) Generate(ctx context.Context, text string) (Audio, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.tts == 0 {
		return Audio{}, apperr.New(apperr.CodeInternal, "tts: engine not initialized")
	}
	sentences := SplitSentences(text)
	if len(sentences) == 0 {
		return Audio{}, apperr.New(apperr.CodeInvalidInput, "tts: 文本中没有可合成的内容")
	}

	var all []float32
	rate := 0
	for i, s := range sentences {
		if err := ctx.Err(); err != nil {
			return Audio{}, apperr.Wrap(apperr.CodeCancelled, "tts: 合成被取消", err)
		}
		samples, sr, err := e.generateOne(s)
		if err != nil {
			return Audio{}, err
		}
		if rate == 0 {
			rate = sr
		} else if sr != rate {
			return Audio{}, apperr.New(apperr.CodeInternal, "tts: 同一文本各句采样率不一致")
		}
		if i > 0 {
			all = append(all, make([]float32, int(float64(rate)*interSentenceSilence))...)
		}
		all = append(all, samples...)
	}

	wav, err := encodeWAV(all, rate)
	if err != nil {
		return Audio{}, err
	}
	dur := time.Duration(float64(len(all)) / float64(rate) * float64(time.Second))
	return Audio{Data: wav, MIME: MIMEWav, Duration: dur}, nil
}

// generateOne 合成单句并把采样复制到 Go 内存（释放 C 侧音频前拷贝）。
func (e *Engine) generateOne(text string) ([]float32, int, error) {
	cText := append([]byte(text), 0)
	textPtr := uintptr(unsafe.Pointer(&cText[0]))
	audioPtr := e.lib.funcs.OfflineTtsGenerate(e.tts, textPtr, 0, e.speed)
	runtime.KeepAlive(cText) // 防 GC 在 FFI 调用期间回收 cText 底层内存
	if audioPtr == 0 {
		return nil, 0, apperr.New(apperr.CodeInternal, "tts: failed to generate audio")
	}
	defer e.lib.funcs.DestroyOfflineTtsGeneratedAudio(audioPtr)

	samplesPtr := *(*uintptr)(unsafe.Pointer(audioPtr))
	n := *(*int32)(unsafe.Pointer(audioPtr + 8))
	sampleRate := *(*int32)(unsafe.Pointer(audioPtr + 12))
	if n <= 0 || samplesPtr == 0 || sampleRate <= 0 {
		return nil, 0, apperr.New(apperr.CodeInternal, "tts: generated audio is empty")
	}
	src := unsafe.Slice((*float32)(unsafe.Pointer(samplesPtr)), n)
	out := make([]float32, n)
	copy(out, src)
	return out, int(sampleRate), nil
}

// Close 实现 Provider 接口，销毁引擎实例。
func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.tts != 0 {
		e.lib.funcs.DestroyOfflineTts(e.tts)
		e.tts = 0
	}
	return nil
}
