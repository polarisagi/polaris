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

// Options 是 Kokoro 引擎的运行参数。
type Options struct {
	// NumThreads 推理线程数；<=0 取 2。
	NumThreads int
	// SID 说话人编号（Kokoro v1.1 的 voices.bin 索引；3 = zf_001 中文女声，0 = af_maple 美音）。
	SID int32
	// Speed 语速倍率；<=0 取 1.0。
	Speed float32
}

// Engine 是 Sherpa-ONNX 本地 TTS 引擎（Kokoro 模型），实现 Provider 接口。
type Engine struct {
	mu    sync.Mutex
	tts   uintptr
	lib   *Library
	sid   int32
	speed float32
}

// Kokoro v1.1 TTS 配置结构体布局（SherpaOnnxOfflineTtsConfig，v1.13.2，总长 448B）。
// 全部偏移按 c-api.h 经 clang offsetof 实测（audio-v2-spec §2.3），升级 sherpa 版本必须重测。
const (
	ttsConfigSize                = 448
	offsetModelNumThreads        = 56
	offsetModelDebug             = 60
	offsetModelProvider          = 64
	offsetModelKokoroModel       = 128
	offsetModelKokoroVoices      = 136
	offsetModelKokoroTokens      = 144
	offsetModelKokoroDataDir     = 152
	offsetModelKokoroLengthScale = 160
	offsetModelKokoroDictDir     = 168 // v1.1 不使用（legacy），置空
	offsetModelKokoroLexicon     = 176
	offsetModelKokoroLang        = 184 // v1.1 自动判定语种，置空
	offsetRuleFsts               = 416
	offsetMaxNumSentences        = 424
	offsetRuleFars               = 432
	offsetSilenceScale           = 440
)

// ruleFstFiles 是 Kokoro 中文文本规整所需的 FST（电话/日期/数字）。
// 缺失时数字、日期会按字面逐字读错（"下午3点"→"下午三点" 失败），所以必须传给引擎。
func ruleFstFiles() []string { return []string{"phone-zh.fst", "date-zh.fst", "number-zh.fst"} }

// NewEngine 构造新的 Sherpa-ONNX 离线 TTS 引擎（Kokoro v1.1）。
// 库未加载或必需文件缺失时返回错误，不构造"空壳引擎"。
func NewEngine(modelDir string, opts Options) (*Engine, error) {
	libMu.Lock()
	lib := libInst
	libMu.Unlock()

	if lib == nil {
		return nil, apperr.New(apperr.CodeInternal, "tts: library not loaded")
	}
	if miss := ModelMissing(modelDir); miss != "" {
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

	// 双语词典顺序：先英文后中文（audio-v2-spec §2.3 实测顺序）。
	lexicon := filepath.Join(modelDir, "lexicon-us-en.txt") + "," + filepath.Join(modelDir, "lexicon-zh.txt")

	var fsts []string
	for _, f := range ruleFstFiles() {
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

	*(*uintptr)(unsafe.Pointer(cfgPtr + offsetModelKokoroModel)) = cString(filepath.Join(modelDir, "model.onnx"))
	*(*uintptr)(unsafe.Pointer(cfgPtr + offsetModelKokoroVoices)) = cString(filepath.Join(modelDir, "voices.bin"))
	*(*uintptr)(unsafe.Pointer(cfgPtr + offsetModelKokoroTokens)) = cString(filepath.Join(modelDir, "tokens.txt"))
	*(*uintptr)(unsafe.Pointer(cfgPtr + offsetModelKokoroDataDir)) = cString(filepath.Join(modelDir, "espeak-ng-data"))
	*(*float32)(unsafe.Pointer(cfgPtr + offsetModelKokoroLengthScale)) = 1.0
	*(*uintptr)(unsafe.Pointer(cfgPtr + offsetModelKokoroDictDir)) = 0
	*(*uintptr)(unsafe.Pointer(cfgPtr + offsetModelKokoroLexicon)) = cString(lexicon)
	*(*uintptr)(unsafe.Pointer(cfgPtr + offsetModelKokoroLang)) = 0

	*(*uintptr)(unsafe.Pointer(cfgPtr + offsetRuleFsts)) = cString(strings.Join(fsts, ","))
	*(*int32)(unsafe.Pointer(cfgPtr + offsetMaxNumSentences)) = 1
	*(*uintptr)(unsafe.Pointer(cfgPtr + offsetRuleFars)) = 0
	*(*float32)(unsafe.Pointer(cfgPtr + offsetSilenceScale)) = 0.2

	tts := lib.funcs.CreateOfflineTts(cfgPtr)
	if tts == 0 {
		return nil, apperr.New(apperr.CodeInternal, "tts: failed to create offline tts engine")
	}

	return &Engine{tts: tts, lib: lib, sid: opts.SID, speed: opts.Speed}, nil
}

// Generate 实现 Provider 接口，生成给定文本的 WAV 音频（ctx 由 sherpa 同步推理忽略）。
func (e *Engine) Generate(_ context.Context, text string) (Audio, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.tts == 0 {
		return Audio{}, apperr.New(apperr.CodeInternal, "tts: engine not initialized")
	}

	cText := append([]byte(text), 0)
	textPtr := uintptr(unsafe.Pointer(&cText[0]))
	audioPtr := e.lib.funcs.OfflineTtsGenerate(e.tts, textPtr, e.sid, e.speed)
	runtime.KeepAlive(cText) // 防 GC 在 FFI 调用期间回收 cText 底层内存
	if audioPtr == 0 {
		return Audio{}, apperr.New(apperr.CodeInternal, "tts: failed to generate audio")
	}
	defer e.lib.funcs.DestroyOfflineTtsGeneratedAudio(audioPtr)

	samplesPtr := *(*uintptr)(unsafe.Pointer(audioPtr))
	n := *(*int32)(unsafe.Pointer(audioPtr + 8))
	sampleRate := *(*int32)(unsafe.Pointer(audioPtr + 12))

	if n <= 0 || samplesPtr == 0 || sampleRate <= 0 {
		return Audio{}, apperr.New(apperr.CodeInternal, "tts: generated audio is empty")
	}

	samples := unsafe.Slice((*float32)(unsafe.Pointer(samplesPtr)), n)

	wav, err := encodeWAV(samples, int(sampleRate))
	if err != nil {
		return Audio{}, err
	}
	dur := time.Duration(float64(n) / float64(sampleRate) * float64(time.Second))
	return Audio{Data: wav, MIME: MIMEWav, Duration: dur}, nil
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
