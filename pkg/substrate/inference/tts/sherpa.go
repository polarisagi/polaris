package tts

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"

	"github.com/polarisagi/polaris/pkg/substrate/inference/stt"
)

var (
	CreateOfflineTts                func(config uintptr) uintptr
	DestroyOfflineTts               func(tts uintptr)
	OfflineTtsGenerate              func(tts uintptr, text uintptr, sid int32, speed float32) uintptr
	DestroyOfflineTtsGeneratedAudio func(audio uintptr)

	loaded  bool
	loadErr error
	mu      sync.Mutex
)

// LoadLibrary 延迟加载 sherpa-onnx 动态库并映射 TTS 符号
func LoadLibrary(libPath string) error {
	mu.Lock()
	defer mu.Unlock()

	if loaded {
		return nil
	}

	lib, err := stt.Dlopen(libPath)
	if err != nil {
		loadErr = err
		return loadErr
	}

	purego.RegisterLibFunc(&CreateOfflineTts, lib, "SherpaOnnxCreateOfflineTts")
	purego.RegisterLibFunc(&DestroyOfflineTts, lib, "SherpaOnnxDestroyOfflineTts")
	purego.RegisterLibFunc(&OfflineTtsGenerate, lib, "SherpaOnnxOfflineTtsGenerate")
	purego.RegisterLibFunc(&DestroyOfflineTtsGeneratedAudio, lib, "SherpaOnnxDestroyOfflineTtsGeneratedAudio")

	loaded = true
	loadErr = nil
	return nil
}

// Engine 包装了 TTS 引擎实例
type Engine struct {
	mu  sync.Mutex
	tts uintptr
}

// NewEngine 构造新的 Sherpa-ONNX 离线 TTS 引擎 (Kokoro 模型)
func NewEngine(modelDir string) (*Engine, error) {
	if !loaded {
		return nil, errors.New("tts: library not loaded")
	}

	const (
		ConfigSize                   = 448
		OffsetModelNumThreads        = 56
		OffsetModelProvider          = 64
		OffsetModelKokoroModel       = 128
		OffsetModelKokoroVoices      = 136
		OffsetModelKokoroTokens      = 144
		OffsetModelKokoroDataDir     = 152
		OffsetModelKokoroLengthScale = 160
		OffsetModelKokoroLexicon     = 176
		OffsetMaxNumSentences        = 424
	)

	configData := make([]byte, ConfigSize)
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

	modelPath := filepath.Join(modelDir, "model.onnx")
	voicesPath := filepath.Join(modelDir, "voices.bin")
	tokensPath := filepath.Join(modelDir, "tokens.txt")
	dataDir := filepath.Join(modelDir, "espeak-ng-data")
	lexiconPath := fmt.Sprintf("%s,%s", filepath.Join(modelDir, "lexicon-zh.txt"), filepath.Join(modelDir, "lexicon-us-en.txt"))

	*(*int32)(unsafe.Pointer(cfgPtr + OffsetModelNumThreads)) = 4
	*(*uintptr)(unsafe.Pointer(cfgPtr + OffsetModelProvider)) = cString("cpu")

	*(*uintptr)(unsafe.Pointer(cfgPtr + OffsetModelKokoroModel)) = cString(modelPath)
	*(*uintptr)(unsafe.Pointer(cfgPtr + OffsetModelKokoroVoices)) = cString(voicesPath)
	*(*uintptr)(unsafe.Pointer(cfgPtr + OffsetModelKokoroTokens)) = cString(tokensPath)
	*(*uintptr)(unsafe.Pointer(cfgPtr + OffsetModelKokoroDataDir)) = cString(dataDir)
	*(*float32)(unsafe.Pointer(cfgPtr + OffsetModelKokoroLengthScale)) = 1.0
	*(*uintptr)(unsafe.Pointer(cfgPtr + OffsetModelKokoroLexicon)) = cString(lexiconPath)

	*(*int32)(unsafe.Pointer(cfgPtr + OffsetMaxNumSentences)) = 1

	tts := CreateOfflineTts(cfgPtr)
	if tts == 0 {
		return nil, errors.New("failed to create offline tts engine")
	}

	return &Engine{tts: tts}, nil
}

// Generate 生成给定文本的音频（返回 WAV 格式二进制流）
func (e *Engine) Generate(text string) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.tts == 0 {
		return nil, errors.New("tts engine not initialized")
	}

	cText := append([]byte(text), 0)
	textPtr := uintptr(unsafe.Pointer(&cText[0]))
	// 强制使用 3 (zf_001，即首个高质量中文女声)。之前使用 0 是 af_maple (纯美音)，导致读中文严重老外口音。
	audioPtr := OfflineTtsGenerate(e.tts, textPtr, 3, 1.0)
	if audioPtr == 0 {
		return nil, errors.New("failed to generate audio")
	}
	defer DestroyOfflineTtsGeneratedAudio(audioPtr)

	samplesPtr := *(*uintptr)(unsafe.Pointer(audioPtr))
	n := *(*int32)(unsafe.Pointer(audioPtr + 8))
	sampleRate := *(*int32)(unsafe.Pointer(audioPtr + 12))

	if n <= 0 || samplesPtr == 0 {
		return nil, errors.New("generated audio is empty")
	}

	samples := unsafe.Slice((*float32)(unsafe.Pointer(samplesPtr)), n)

	return encodeWAV(samples, int(sampleRate))
}

// Close 销毁引擎实例
func (e *Engine) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.tts != 0 {
		DestroyOfflineTts(e.tts)
		e.tts = 0
	}
}

// encodeWAV 将 float32 数组转换为单声道 16-bit PCM 的 WAV 文件
func encodeWAV(samples []float32, sampleRate int) ([]byte, error) {
	var buf bytes.Buffer
	numSamples := len(samples)
	dataSize := numSamples * 2
	fileSize := 36 + dataSize

	buf.WriteString("RIFF")
	if err := binary.Write(&buf, binary.LittleEndian, int32(fileSize)); err != nil {
		return nil, err
	}
	buf.WriteString("WAVE")

	buf.WriteString("fmt ")
	if err := binary.Write(&buf, binary.LittleEndian, int32(16)); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.LittleEndian, int16(1)); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.LittleEndian, int16(1)); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.LittleEndian, int32(sampleRate)); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.LittleEndian, int32(sampleRate*2)); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.LittleEndian, int16(2)); err != nil {
		return nil, err
	}
	if err := binary.Write(&buf, binary.LittleEndian, int16(16)); err != nil {
		return nil, err
	}

	buf.WriteString("data")
	if err := binary.Write(&buf, binary.LittleEndian, int32(dataSize)); err != nil {
		return nil, err
	}

	for _, sample := range samples {
		// float32 [-1, 1] -> int16 [-32768, 32767]
		s := int16(math.Max(-32768, math.Min(32767, float64(sample)*32767.0)))
		if err := binary.Write(&buf, binary.LittleEndian, s); err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}
