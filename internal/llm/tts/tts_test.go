package tts

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── encodeWAV ──────────────────────────────────────────────────────────────

func TestEncodeWAV_Header(t *testing.T) {
	// 1kHz 正弦，4 个采样
	sampleRate := 22050
	samples := []float32{0.0, 0.5, 1.0, -1.0}

	data, err := encodeWAV(samples, sampleRate)
	if err != nil {
		t.Fatalf("encodeWAV: %v", err)
	}

	// WAV = RIFF header(44 bytes) + PCM data(n*2 bytes)
	want := 44 + len(samples)*2
	if len(data) != want {
		t.Errorf("len(data) = %d, want %d", len(data), want)
	}

	r := bytes.NewReader(data)

	// RIFF chunk
	riff := make([]byte, 4)
	if _, err := r.Read(riff); err != nil || string(riff) != "RIFF" {
		t.Errorf("expected RIFF, got %q", riff)
	}

	var fileSize int32
	_ = binary.Read(r, binary.LittleEndian, &fileSize)
	if int(fileSize) != 36+len(samples)*2 {
		t.Errorf("fileSize = %d, want %d", fileSize, 36+len(samples)*2)
	}

	wave := make([]byte, 4)
	if _, err := r.Read(wave); err != nil || string(wave) != "WAVE" {
		t.Errorf("expected WAVE, got %q", wave)
	}

	// fmt chunk
	fmt_ := make([]byte, 4)
	if _, err := r.Read(fmt_); err != nil || string(fmt_) != "fmt " {
		t.Errorf("expected 'fmt ', got %q", fmt_)
	}

	var chunkSize int32
	_ = binary.Read(r, binary.LittleEndian, &chunkSize)
	if chunkSize != 16 {
		t.Errorf("fmt chunkSize = %d, want 16", chunkSize)
	}

	var audioFmt int16
	_ = binary.Read(r, binary.LittleEndian, &audioFmt)
	if audioFmt != 1 { // PCM
		t.Errorf("audioFmt = %d, want 1 (PCM)", audioFmt)
	}

	var numChannels int16
	_ = binary.Read(r, binary.LittleEndian, &numChannels)
	if numChannels != 1 {
		t.Errorf("numChannels = %d, want 1", numChannels)
	}

	var sr int32
	_ = binary.Read(r, binary.LittleEndian, &sr)
	if sr != int32(sampleRate) {
		t.Errorf("sampleRate = %d, want %d", sr, sampleRate)
	}
}

func TestEncodeWAV_Clipping(t *testing.T) {
	// 超出 [-1, 1] 范围的值必须被截断到 int16 边界
	samples := []float32{2.0, -2.0}
	data, err := encodeWAV(samples, 22050)
	if err != nil {
		t.Fatalf("encodeWAV: %v", err)
	}

	// PCM 数据从偏移 44 开始
	r := bytes.NewReader(data[44:])
	var s0, s1 int16
	_ = binary.Read(r, binary.LittleEndian, &s0)
	_ = binary.Read(r, binary.LittleEndian, &s1)

	if s0 != 32767 {
		t.Errorf("clamp +2.0 → %d, want 32767", s0)
	}
	if s1 != -32768 {
		t.Errorf("clamp -2.0 → %d, want -32768", s1)
	}
}

func TestEncodeWAV_PrecisionMidValue(t *testing.T) {
	// encodeWAV 用 int16(float64(sample)*32767.0) 截断（非四舍五入）：
	// 0.5 * 32767.0 = 16383.5 → int16 截断 → 16383
	samples := []float32{0.5}
	data, err := encodeWAV(samples, 16000)
	if err != nil {
		t.Fatalf("encodeWAV: %v", err)
	}
	var s int16
	_ = binary.Read(bytes.NewReader(data[44:]), binary.LittleEndian, &s)
	var v float32 = 0.5
	want := int16(float64(v) * 32767.0) // 运行时求值，与 encodeWAV 截断逻辑一致
	if s != want {
		t.Errorf("0.5 → %d, want %d", s, want)
	}
}

// ── LoadLibrary ────────────────────────────────────────────────────────────

func TestLoadLibrary_NonexistentPath(t *testing.T) {
	// 重置状态，防止包级 loaded 标志影响本测试
	mu.Lock()
	wasLoaded := loaded
	loadedBefore := loaded
	mu.Unlock()

	if loadedBefore {
		t.Skip("library already loaded in this process, cannot test failure path")
	}

	err := LoadLibrary("/nonexistent/path/to/libsherpa.so")
	if err == nil {
		t.Error("expected error for nonexistent library path, got nil")
	}

	// 恢复状态
	mu.Lock()
	if !wasLoaded {
		loaded = false
		loadErr = nil
	}
	mu.Unlock()
}

func TestNewEngine_NotLoaded(t *testing.T) {
	mu.Lock()
	wasLoaded := loaded
	loaded = false
	mu.Unlock()
	defer func() {
		mu.Lock()
		loaded = wasLoaded
		mu.Unlock()
	}()

	_, err := NewEngine("/some/model/dir")
	if err == nil {
		t.Error("expected error when library not loaded, got nil")
	}
}

// ── ttsModelPresent ────────────────────────────────────────────────────────

func TestTTSModelPresent_NoDirOrEmpty(t *testing.T) {
	if ttsModelPresent("/nonexistent/dir") {
		t.Error("expected false for nonexistent directory")
	}

	dir := t.TempDir()
	if ttsModelPresent(dir) {
		t.Error("expected false for empty directory")
	}
}

func TestTTSModelPresent_WithOnnxFile(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "model.onnx"))
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	if !ttsModelPresent(dir) {
		t.Error("expected true when .onnx file present")
	}
}

// ── ttsModelMapper ─────────────────────────────────────────────────────────

func TestTTSModelMapper_OnnxFile(t *testing.T) {
	mapper := ttsModelMapper("/models")
	path, ok := mapper("kokoro/model.onnx")
	if !ok {
		t.Error("expected .onnx to be accepted")
	}
	if !strings.HasSuffix(path, "model.onnx") {
		t.Errorf("unexpected path: %q", path)
	}
}

func TestTTSModelMapper_EspeakNgData(t *testing.T) {
	mapper := ttsModelMapper("/models")
	path, ok := mapper("kokoro/espeak-ng-data/en/rules")
	if !ok {
		t.Error("expected espeak-ng-data to be accepted")
	}
	if !strings.Contains(path, "espeak-ng-data") {
		t.Errorf("expected espeak-ng-data in path, got %q", path)
	}
}

func TestTTSModelMapper_UnknownFile(t *testing.T) {
	mapper := ttsModelMapper("/models")
	_, ok := mapper("kokoro/README.md")
	if ok {
		t.Error("expected README.md to be rejected")
	}
}

// ── ModelDir ───────────────────────────────────────────────────────────────

func TestModelDir(t *testing.T) {
	got := ModelDir("/tts")
	if got != "/tts/model" {
		t.Errorf("got %q, want %q", got, "/tts/model")
	}
}
