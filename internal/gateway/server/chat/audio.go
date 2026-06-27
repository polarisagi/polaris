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

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/tool/builtin"

	"github.com/polarisagi/polaris/internal/llm/stt"
	"github.com/polarisagi/polaris/internal/llm/tts"
)

// SetSTTEngine 原子替换全局 STT 引擎实例（goroutine-safe）。
func (h *ChatHandler) SetSTTEngine(engine *stt.Engine) {
	h.STTEngine.Store(engine)
}

// SetTTSEngine 原子替换全局 TTS Provider 实例（goroutine-safe）。
// p == nil 时显式清除（使 Load 返回 nil，HandleAudioSpeech 返回 503）。
func (h *ChatHandler) SetTTSEngine(p tts.Provider) {
	if p == nil {
		h.TTSEngine.Store(nil)
		return
	}
	h.TTSEngine.Store(&tts.ProviderBox{P: p})
}

func (h *ChatHandler) HandleAudioSpeech(w http.ResponseWriter, r *http.Request) {
	box := h.TTSEngine.Load()
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

// handleAudioTranscriptions 处理前端语音输入并转写文本
// 路由: POST /v1/audio/transcriptions
func (h *ChatHandler) HandleAudioTranscriptions(w http.ResponseWriter, r *http.Request) {
	// 原子 Load，与 SetSTTEngine 的 Store 不存在 data race
	engine := h.STTEngine.Load()
	if engine == nil {
		http.Error(w, "STT Engine not initialized", http.StatusServiceUnavailable)
		return
	}

	// 解析 multipart，获取 audio 文件 (通常是 webm 格式)
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

	// 将录音保存为临时文件以供 ffmpeg 处理
	// 使用上传文件的真实扩展名（.webm/.mp4/.ogg），ffmpeg 凭文件头自动识别格式
	tmpDir := os.TempDir()
	ext := filepath.Ext(header.Filename)
	if ext == "" {
		ext = ".webm" // 兜底
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

	// 使用 ffmpeg 提取为 16000Hz f32le 原始 PCM 数据流
	// 不落地 wav 文件，直接通过管道读取标准输出
	pcmBytes, err := builtin.ConvertToRawPCM(r.Context(), inPath)
	if err != nil {
		slog.Error("ffmpeg decode failed", "err", err)
		mockRes, _ := engine.Transcribe(nil, 16000)
		respondJSON(w, mockRes)
		return
	}
	samples := make([]float32, len(pcmBytes)/4)
	for i := range samples {
		bits := binary.LittleEndian.Uint32(pcmBytes[i*4 : (i+1)*4])
		samples[i] = math.Float32frombits(bits)
	}

	res, err := engine.Transcribe(samples, 16000)
	if err != nil {
		http.Error(w, "stt failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	respondJSON(w, res)
}

func respondJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}
