package chat

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/llm/stt"
	"github.com/polarisagi/polaris/internal/tool/builtin"
)

type AudioService struct {
	STTEngine  *atomic.Pointer[STTEngineBox]
	TTSEngine  *atomic.Pointer[TTSProviderBox]
	BinDir     string
	HTTPClient *http.Client
}

func NewAudioService(stt *atomic.Pointer[STTEngineBox], tts *atomic.Pointer[TTSProviderBox]) *AudioService {
	return &AudioService{
		STTEngine: stt,
		TTSEngine: tts,
	}
}

// SetBinDir 设置二进制可执行文件查找目录（用于查找或自动安装 ffmpeg）。
func (s *AudioService) SetBinDir(binDir string) {
	s.BinDir = binDir
}

// SetHTTPClient 设置安全 HTTP 客户端（用于下载 ffmpeg 等资产）。
func (s *AudioService) SetHTTPClient(client *http.Client) {
	s.HTTPClient = client
}

// SetSTTEngine 原子替换全局 STT 引擎实例（goroutine-safe）。
// engine 为 nil 时显式清除（使 Load 后 E 字段为 nil，HandleAudioTranscriptions 返回 503）。
func (s *AudioService) SetSTTEngine(engine STTTranscriber) {
	s.STTEngine.Store(&STTEngineBox{E: engine})
}

// SetTTSEngine 原子替换全局 TTS Provider 实例（goroutine-safe）。
// p == nil 时显式清除（使 Load 返回 nil，HandleAudioSpeech 返回 503）。
func (s *AudioService) SetTTSEngine(p TTSProvider) {
	if p == nil {
		s.TTSEngine.Store(nil)
		return
	}
	s.TTSEngine.Store(&TTSProviderBox{P: p})
}

func (s *AudioService) HandleAudioSpeech(w http.ResponseWriter, r *http.Request) {
	box := s.TTSEngine.Load()
	if box == nil {
		http.Error(w, "TTS Engine not initialized", http.StatusServiceUnavailable)
		return
	}

	var req struct {
		Input string `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Input == "" {
		http.Error(w, "input is empty", http.StatusBadRequest)
		return
	}

	wavData, err := box.P.Generate(r.Context(), req.Input)
	if err != nil {
		slog.Error("audio: tts generation failed", "err", err)
		http.Error(w, "internal server error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(wavData)))
	if _, err := w.Write(wavData); err != nil {
		slog.Warn("audio: failed to write response", "err", err)
	}
}

// HandleAudioTranscriptions 处理前端语音输入并转写文本。
// 路由: POST /v1/audio/transcriptions
//
// 架构分流策略：
//  1. 若上传音频为 .wav 格式（标准录音）：走纯 Go 内存解码器，零依赖，直接送入 STT 引擎；
//  2. 若为非 .wav 格式（.webm/.mp4/.ogg/.mp3/.flac 等）：按 binDir → PATH 顺序查找 ffmpeg，
//     若不存在则自动按平台下载；转码失败返回明确 422 错误，绝不静默返回假数据。
func (s *AudioService) HandleAudioTranscriptions(w http.ResponseWriter, r *http.Request) {
	// 原子 Load，与 SetSTTEngine 的 Store 不存在 data race
	box := s.STTEngine.Load()
	if box == nil || box.E == nil {
		http.Error(w, "STT Engine not initialized", http.StatusServiceUnavailable)
		return
	}
	engine := box.E

	// 解析 multipart，获取 audio 文件 (通常是 wav 或 webm 格式)
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20) // 最大 10MB
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// 保存为临时文件
	tmpDir := os.TempDir()
	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext == "" {
		ext = ".wav" // 默认优先尝试 wav
	}
	inPath := filepath.Join(tmpDir, uuid.New().String()+ext)

	outFile, err := os.Create(inPath)
	if err != nil {
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	if _, err := io.Copy(outFile, file); err != nil {
		outFile.Close()
		http.Error(w, "server error", http.StatusInternalServerError)
		return
	}
	outFile.Close()
	defer os.Remove(inPath)

	var samples []float32
	var sampleRate int

	switch ext {
	case ".wav":
		// 路径 A：纯 Go WAV 解码器（零外部依赖，极速且健壮）
		f, err := os.Open(inPath)
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		defer f.Close()

		wavSamples, sr, err := stt.DecodeWAV(f)
		if err != nil {
			slog.Warn("wav decode failed", "err", err)
			httputil.WriteJSONStatus(w, http.StatusBadRequest, map[string]string{
				"error":   "invalid_wav",
				"message": "WAV 文件解码失败: " + err.Error(),
			})
			return
		}
		samples = wavSamples
		sampleRate = sr

	default:
		// 路径 B：非 WAV 格式需通过 ffmpeg 转码
		ffmpegExe, err := builtin.EnsureFFmpeg(r.Context(), s.BinDir, s.HTTPClient)
		if err != nil {
			slog.Error("audio decode failed: ffmpeg unavailable", "ext", ext, "err", err)
			httputil.WriteJSONStatus(w, http.StatusUnprocessableEntity, map[string]string{
				"error":   "ffmpeg_not_found",
				"message": "音频解码失败：系统未安装 ffmpeg 且自动安装失败，无法直接解析非 WAV 格式音频。请安装 ffmpeg 或使用标准 WAV 录音。",
			})
			return
		}

		pcmBytes, err := builtin.ConvertToRawPCM(r.Context(), inPath, ffmpegExe)
		if err != nil {
			slog.Error("ffmpeg decode failed", "err", err)
			httputil.WriteJSONStatus(w, http.StatusUnprocessableEntity, map[string]string{
				"error":   "audio_transcode_failed",
				"message": "音频转码失败：ffmpeg 无法识别或转码上传的文件，请检查音频格式。",
			})
			return
		}

		samples = make([]float32, len(pcmBytes)/4)
		for i := range samples {
			bits := binary.LittleEndian.Uint32(pcmBytes[i*4 : (i+1)*4])
			samples[i] = math.Float32frombits(bits)
		}
		sampleRate = 16000
	}

	res, err := engine.Transcribe(samples, sampleRate)
	if err != nil {
		http.Error(w, "stt failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	respondJSON(w, res)
}

func respondJSON(w http.ResponseWriter, data any) {
	httputil.WriteJSON(w, data)
}
