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
	"golang.org/x/sync/singleflight"

	"github.com/polarisagi/polaris/internal/gateway/httputil"
	"github.com/polarisagi/polaris/internal/llm/stt"
	"github.com/polarisagi/polaris/internal/tool/builtin"
)

type AudioService struct {
	STTEngine  *atomic.Pointer[STTEngineBox]
	TTSEngine  *atomic.Pointer[TTSProviderBox]
	binDir     string
	httpClient *http.Client

	// STTStatus / TTSStatus 资产状态机快照（经 /v1/system/capabilities 暴露给前端）。
	STTStatus *AudioStatusTracker
	TTSStatus *AudioStatusTracker
	// sttRetry 容量 1 的唤醒信号：STT 处于 failed 状态时，转写请求非阻塞地投递一次，
	// 让后台重试循环跳过退避立即重试；重复信号直接丢弃。
	sttRetry chan struct{}

	ffmpegSF singleflight.Group
}

func NewAudioService(stt *atomic.Pointer[STTEngineBox], tts *atomic.Pointer[TTSProviderBox], binDir string, httpClient *http.Client) *AudioService {
	return &AudioService{
		STTEngine:  stt,
		TTSEngine:  tts,
		binDir:     binDir,
		httpClient: httpClient,
		STTStatus:  NewAudioStatusTracker("stt"),
		TTSStatus:  NewAudioStatusTracker("tts"),
		sttRetry:   make(chan struct{}, 1),
	}
}

// STTRetrySignal 返回 STT 重试唤醒通道（只读端），供后台准备循环 select。
func (s *AudioService) STTRetrySignal() <-chan struct{} { return s.sttRetry }

// kickSTTRetry 非阻塞投递一次重试信号（通道满则丢弃，不堆积）。
func (s *AudioService) kickSTTRetry() {
	select {
	case s.sttRetry <- struct{}{}:
	default:
	}
}

// SetSTTEngine 原子替换全局 STT 引擎实例（goroutine-safe）。
// engine 为 nil 时显式清除（使 Load 后 E 字段为 nil，HandleAudioTranscriptions 返回 503）。
func (s *AudioService) SetSTTEngine(engine STTTranscriber) {
	s.STTEngine.Store(&STTEngineBox{E: engine})
}

// SetTTSEngine 原子替换全局 TTS Provider 实例（goroutine-safe）。
// p == nil 时显式清除（使 Load 返回 nil，HandleAudioSpeech 返回 503）。
func (s *AudioService) SetTTSEngine(p TTSProvider, name string) {
	if p == nil {
		s.TTSEngine.Store(nil)
		return
	}
	s.TTSEngine.Store(&TTSProviderBox{P: p, Name: name})
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

	audio, err := box.P.Generate(r.Context(), req.Input)
	if err != nil {
		slog.Error("audio: tts generation failed", "err", err)
		http.Error(w, "internal server error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// MIME 由 Provider 给出（Edge=audio/mpeg，Sherpa=audio/wav）；缺省按 wav 兜底。
	mime := audio.MIME
	if mime == "" {
		mime = "audio/wav"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(audio.Data)))
	if _, err := w.Write(audio.Data); err != nil {
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
		s.writeSTTNotReady(w)
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

	// 架构分流策略：
	//  1. 若上传音频为 .wav 格式（标准录音）：走纯 Go 内存解码器，零依赖，直接送入 STT 引擎；
	//  2. 若为非 .wav 格式（.webm/.mp4/.ogg/.mp3/.flac 等）：按 binDir → PATH 顺序查找 ffmpeg，
	//     若不存在则自动按平台下载；转码失败返回明确 422 错误，绝不静默返回假数据。

	tmpDir := os.TempDir()
	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext == "" {
		ext = ".wav" // 默认优先尝试 wav
	}

	var samples []float32
	var sampleRate int

	switch ext {
	case ".wav":
		// 路径 A：纯 Go WAV 解码器（零外部依赖，直接从 multipart stream 解码）
		wavSamples, sr, err := stt.DecodeWAV(file)
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
		// 路径 B：非 WAV 格式需通过 ffmpeg 转码，必须落盘为临时文件
		inPath := filepath.Join(tmpDir, uuid.New().String()+ext)
		outFile, err := os.Create(inPath)
		if err != nil {
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		if _, err := io.Copy(outFile, file); err != nil {
			outFile.Close()
			os.Remove(inPath)
			http.Error(w, "server error", http.StatusInternalServerError)
			return
		}
		outFile.Close()
		defer os.Remove(inPath)

		// 使用 singleflight 保护并发下载
		v, err, _ := s.ffmpegSF.Do("ffmpeg", func() (any, error) {
			return builtin.EnsureFFmpeg(r.Context(), s.binDir, s.httpClient)
		})
		if err != nil {
			slog.Error("audio decode failed: ffmpeg unavailable", "ext", ext, "err", err)
			httputil.WriteJSONStatus(w, http.StatusUnprocessableEntity, map[string]string{
				"error":   "ffmpeg_not_found",
				"message": "音频解码失败：系统未安装 ffmpeg 且自动安装失败，无法直接解析非 WAV 格式音频。请安装 ffmpeg 或使用标准 WAV 录音。",
			})
			return
		}
		ffmpegExe := v.(string)

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
		slog.Error("audio: stt transcribe failed", "err", err)
		httputil.WriteJSONStatus(w, http.StatusInternalServerError, map[string]string{
			"error":   "stt_failed",
			"message": "语音识别失败: " + err.Error(),
		})
		return
	}

	respondJSON(w, res)
}

// writeSTTNotReady 返回 503 JSON，携带状态机的 state/detail/message，让前端能说清"为什么现在不能用"。
func (s *AudioService) writeSTTNotReady(w http.ResponseWriter) {
	st := s.STTStatus.Get()
	if st.State == AudioStateFailed {
		s.kickSTTRetry() // 用户正在使用：跳过退避立即重试一次
	}
	msg := "语音识别引擎尚未就绪"
	if st.Detail != "" {
		msg += "：" + st.Detail
	}
	if st.Error != "" {
		msg += "（" + st.Error + "）"
	}
	httputil.WriteJSONStatus(w, http.StatusServiceUnavailable, map[string]string{
		"error":   "stt_not_ready",
		"state":   st.State,
		"detail":  st.Detail,
		"message": msg,
	})
}

func respondJSON(w http.ResponseWriter, data any) {
	httputil.WriteJSON(w, data)
}
