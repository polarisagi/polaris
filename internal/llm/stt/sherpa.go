package stt

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// SherpaOnnxOfflineRecognizer is an opaque pointer
type SherpaOnnxOfflineRecognizer struct{}

// SherpaOnnxOfflineStream is an opaque pointer
type SherpaOnnxOfflineStream struct{}

// SherpaOnnxOfflinePunctuation is an opaque pointer
type SherpaOnnxOfflinePunctuation struct{}

type sherpaFuncs struct {
	CreateOfflineRecognizer        func(config uintptr) *SherpaOnnxOfflineRecognizer
	DestroyOfflineRecognizer       func(recognizer *SherpaOnnxOfflineRecognizer)
	CreateOfflineStream            func(recognizer *SherpaOnnxOfflineRecognizer) *SherpaOnnxOfflineStream
	DestroyOfflineStream           func(stream *SherpaOnnxOfflineStream)
	AcceptWaveformOffline          func(stream *SherpaOnnxOfflineStream, sampleRate int32, samples *float32, n int32)
	DecodeOfflineStream            func(recognizer *SherpaOnnxOfflineRecognizer, stream *SherpaOnnxOfflineStream)
	GetOfflineStreamResult         func(stream *SherpaOnnxOfflineStream) uintptr
	DestroyOfflineRecognizerResult func(result uintptr)

	CreateOfflinePunctuation   func(config uintptr) *SherpaOnnxOfflinePunctuation
	DestroyOfflinePunctuation  func(punct *SherpaOnnxOfflinePunctuation)
	OfflinePunctuationAddPunct func(punct *SherpaOnnxOfflinePunctuation, text uintptr) uintptr
	OfflinePunctuationFreeText func(text uintptr)
}

// Library 封装 sherpa-onnx 函数指针，避免包级全局可变变量
type Library struct {
	funcs sherpaFuncs
}

var (
	libMu   sync.Mutex
	loadErr error
	libInst *Library
)

// LoadLibrary 动态加载 sherpa-onnx C API (零 CGO)。
// 幂等可重入：已加载则直接返回 nil；加载失败后可再次尝试（下载完成后调用）。
func LoadLibrary(libPath string) error {
	libMu.Lock()
	defer libMu.Unlock()

	if libInst != nil {
		return nil // 已成功加载，直接复用
	}

	lib, err := Dlopen(libPath)
	if err != nil {
		loadErr = err
		return loadErr
	}

	var sf sherpaFuncs
	purego.RegisterLibFunc(&sf.CreateOfflineRecognizer, lib, "SherpaOnnxCreateOfflineRecognizer")
	purego.RegisterLibFunc(&sf.DestroyOfflineRecognizer, lib, "SherpaOnnxDestroyOfflineRecognizer")
	purego.RegisterLibFunc(&sf.CreateOfflineStream, lib, "SherpaOnnxCreateOfflineStream")
	purego.RegisterLibFunc(&sf.DestroyOfflineStream, lib, "SherpaOnnxDestroyOfflineStream")
	purego.RegisterLibFunc(&sf.AcceptWaveformOffline, lib, "SherpaOnnxAcceptWaveformOffline")
	purego.RegisterLibFunc(&sf.DecodeOfflineStream, lib, "SherpaOnnxDecodeOfflineStream")
	purego.RegisterLibFunc(&sf.GetOfflineStreamResult, lib, "SherpaOnnxGetOfflineStreamResult")
	purego.RegisterLibFunc(&sf.DestroyOfflineRecognizerResult, lib, "SherpaOnnxDestroyOfflineRecognizerResult")

	purego.RegisterLibFunc(&sf.CreateOfflinePunctuation, lib, "SherpaOnnxCreateOfflinePunctuation")
	purego.RegisterLibFunc(&sf.DestroyOfflinePunctuation, lib, "SherpaOnnxDestroyOfflinePunctuation")
	purego.RegisterLibFunc(&sf.OfflinePunctuationAddPunct, lib, "SherpaOfflinePunctuationAddPunct")
	purego.RegisterLibFunc(&sf.OfflinePunctuationFreeText, lib, "SherpaOfflinePunctuationFreeText")

	libInst = &Library{funcs: sf}
	loadErr = nil
	return nil
}

// Engine 包装了 STT 引擎实例
type Engine struct {
	mu         sync.Mutex
	recognizer *SherpaOnnxOfflineRecognizer
	punct      *SherpaOnnxOfflinePunctuation
	lib        *Library
}

// NewEngine 构造新的 Sherpa-ONNX 离线推理引擎。
// 库未加载时返回错误而非"空壳引擎"：空壳会让调用方误以为引擎已就绪，并在 Transcribe
// 时回出假文本（S7）。
//
// useITN 控制 SenseVoice 的逆文本规范化。实测（官方 sherpa-onnx-offline 同模型同参数
// 输出与本实现逐字节一致）itn=1 会丢首字、错词（zh.wav "开放时间…"→"放时间…"），
// 属模型 ITN 路径缺陷而非 FFI 问题，故默认应传 false。
func NewEngine(modelDir, punctDir, language string, numThreads int, useITN bool) (*Engine, error) {
	libMu.Lock()
	lib := libInst
	libMu.Unlock()

	if lib == nil {
		return nil, apperr.New(apperr.CodeUnimplemented, "stt: sherpa-onnx library not loaded")
	}

	if language == "" {
		language = "zh"
	}
	if numThreads <= 0 {
		numThreads = 1
	}

	// 动态构造 SherpaOnnxOfflineRecognizerConfig (v1.13.8 布局；已按 1.13.8 c-api.h 复测，与 1.13.2 逐项一致)
	const (
		ConfigSize                    = 608
		OffsetFeatSampleRate          = 0
		OffsetFeatFeatureDim          = 4
		OffsetModelTokens             = 104
		OffsetModelNumThreads         = 112
		OffsetModelDebug              = 116
		OffsetModelProvider           = 120
		OffsetModelSenseVoiceModel    = 160
		OffsetModelSenseVoiceLanguage = 168
		OffsetModelSenseVoiceUseItn   = 176
		OffsetDecodingMethod          = 528
	)

	configData := make([]byte, ConfigSize)
	cfgPtr := uintptr(unsafe.Pointer(&configData[0]))

	var refs [][]byte
	cString := func(s string) uintptr {
		b := append([]byte(s), 0)
		refs = append(refs, b)
		return uintptr(unsafe.Pointer(&b[0]))
	}
	defer runtime.KeepAlive(refs)
	defer runtime.KeepAlive(configData)

	// FeatConfig
	*(*int32)(unsafe.Pointer(cfgPtr + OffsetFeatSampleRate)) = 16000
	*(*int32)(unsafe.Pointer(cfgPtr + OffsetFeatFeatureDim)) = 80

	// ModelConfig (SenseVoice)
	modelPath := filepath.Join(modelDir, "model.onnx")
	tokensPath := filepath.Join(modelDir, "tokens.txt")
	*(*uintptr)(unsafe.Pointer(cfgPtr + OffsetModelSenseVoiceModel)) = cString(modelPath)
	*(*uintptr)(unsafe.Pointer(cfgPtr + OffsetModelSenseVoiceLanguage)) = cString(language)
	var itn int32
	if useITN {
		itn = 1
	}
	*(*int32)(unsafe.Pointer(cfgPtr + OffsetModelSenseVoiceUseItn)) = itn
	*(*uintptr)(unsafe.Pointer(cfgPtr + OffsetModelTokens)) = cString(tokensPath)
	*(*int32)(unsafe.Pointer(cfgPtr + OffsetModelNumThreads)) = int32(numThreads)
	*(*int32)(unsafe.Pointer(cfgPtr + OffsetModelDebug)) = 0
	*(*uintptr)(unsafe.Pointer(cfgPtr + OffsetModelProvider)) = cString("cpu")

	// DecodingMethod
	*(*uintptr)(unsafe.Pointer(cfgPtr + OffsetDecodingMethod)) = cString("greedy_search")

	// 调用 C API
	rec := lib.funcs.CreateOfflineRecognizer(cfgPtr)
	if rec == nil {
		return nil, apperr.New(apperr.CodeInternal, "stt: failed to create offline recognizer")
	}

	// 实例化 Punctuation 模型（若有）
	// 仅当 model.onnx 实际存在才创建：把不存在的路径传进 C 侧会得到不可诊断的失败。
	var punct *SherpaOnnxOfflinePunctuation
	punctModelPath := filepath.Join(punctDir, "model.onnx")
	if punctDir != "" {
		if _, statErr := os.Stat(punctModelPath); statErr != nil {
			slog.Warn("stt: punctuation model missing, continuing without punctuation", "path", punctModelPath)
			punctDir = ""
		}
	}
	if punctDir != "" {
		const PunctConfigSize = 24
		const PunctOffsetModel = 0
		const PunctOffsetNumThreads = 8
		const PunctOffsetDebug = 12
		const PunctOffsetProvider = 16

		punctConfigData := make([]byte, PunctConfigSize)
		pCfgPtr := uintptr(unsafe.Pointer(&punctConfigData[0]))
		defer runtime.KeepAlive(punctConfigData)

		*(*uintptr)(unsafe.Pointer(pCfgPtr + PunctOffsetModel)) = cString(punctModelPath)
		*(*int32)(unsafe.Pointer(pCfgPtr + PunctOffsetNumThreads)) = int32(numThreads)
		*(*int32)(unsafe.Pointer(pCfgPtr + PunctOffsetDebug)) = 0
		*(*uintptr)(unsafe.Pointer(pCfgPtr + PunctOffsetProvider)) = cString("cpu")

		punct = lib.funcs.CreateOfflinePunctuation(pCfgPtr)
		if punct == nil {
			// 标点只是后处理增强，创建失败不应阻断 STT。
			slog.Warn("stt: CreateOfflinePunctuation returned nil, continuing without punctuation", "path", punctModelPath)
		}
	}

	return &Engine{
		recognizer: rec,
		punct:      punct,
		lib:        lib,
	}, nil
}

// Close 释放 C 引擎资源
func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.lib == nil {
		return
	}

	if e.recognizer != nil {
		e.lib.funcs.DestroyOfflineRecognizer(e.recognizer)
		e.recognizer = nil
	}
	if e.punct != nil {
		e.lib.funcs.DestroyOfflinePunctuation(e.punct)
		e.punct = nil
	}
}

// Result 包含语音识别的文字和扩展（语言、情感、事件）信息
type Result struct {
	Text    string `json:"text"`
	Lang    string `json:"lang"`
	Emotion string `json:"emotion"`
	Event   string `json:"event"`
}

// Transcribe 传入 16000Hz 16-bit PCM 单声道音频数据并返回文本。
// 引擎未初始化时返回错误，绝不回出假文本（S7）。
func (e *Engine) Transcribe(samples []float32, sampleRate int) (Result, error) {
	if e == nil || e.lib == nil || e.recognizer == nil {
		return Result{}, apperr.New(apperr.CodeUnimplemented, "stt: engine not initialized")
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	stream := e.lib.funcs.CreateOfflineStream(e.recognizer)
	if stream == nil {
		return Result{}, apperr.New(apperr.CodeInternal, "stt: failed to create stream")
	}
	defer e.lib.funcs.DestroyOfflineStream(stream)

	if len(samples) > 0 {
		e.lib.funcs.AcceptWaveformOffline(stream, int32(sampleRate), &samples[0], int32(len(samples)))
	}
	e.lib.funcs.DecodeOfflineStream(e.recognizer, stream)

	resPtr := e.lib.funcs.GetOfflineStreamResult(stream)
	if resPtr == 0 {
		return Result{}, apperr.New(apperr.CodeInternal, "stt: failed to get result")
	}
	defer e.lib.funcs.DestroyOfflineRecognizerResult(resPtr)

	// 解析返回的 C 结构体中的 const char* text
	// 按照 SherpaOnnx 规范，result 的第一个字段就是 text 指针
	textPtr := *(**byte)(unsafe.Pointer(resPtr))
	if textPtr == nil {
		return Result{}, nil
	}

	// 提取 Lang / Emotion / Event。偏移取自 sherpa-onnx v1.13.2/v1.13.8 c-api.h（两版一致） 经 clang
	// offsetof 实测：json=40, lang=48, emotion=56, event=64。
	// 旧代码按 emotion@40 / event@48 读取，实际装进去的是整段 JSON 与语言标签。
	const (
		offsetResultLang    = 48
		offsetResultEmotion = 56
		offsetResultEvent   = 64
	)
	cStrAt := func(off uintptr) string {
		p := *(**byte)(unsafe.Pointer(resPtr + off))
		if p == nil {
			return ""
		}
		return parseCString(uintptr(unsafe.Pointer(p)))
	}
	langText := cStrAt(offsetResultLang)
	emotionText := cStrAt(offsetResultEmotion)
	eventText := cStrAt(offsetResultEvent)

	// 简单的 C 字符串转 Go 字符串
	rawText := parseCString(uintptr(unsafe.Pointer(textPtr)))

	if e.punct != nil && rawText != "" && e.lib != nil {
		cRawText := append([]byte(rawText), 0)
		cRawPtr := uintptr(unsafe.Pointer(&cRawText[0]))
		punctuatedPtr := e.lib.funcs.OfflinePunctuationAddPunct(e.punct, cRawPtr)
		if punctuatedPtr != 0 {
			defer e.lib.funcs.OfflinePunctuationFreeText(punctuatedPtr)
			rawText = parseCString(punctuatedPtr)
		}
		runtime.KeepAlive(cRawText)
	}

	return Result{Text: rawText, Lang: langText, Emotion: emotionText, Event: eventText}, nil
}

func parseCString(ptr uintptr) string {
	if ptr == 0 {
		return ""
	}
	var bytes []byte
	for i := 0; ; i++ {
		b := *(*byte)(unsafe.Pointer(ptr + uintptr(i)))
		if b == 0 {
			break
		}
		bytes = append(bytes, b)
	}
	return string(bytes)
}
