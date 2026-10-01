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
	// 重置状态，防止包级 libInst 单例影响本测试（ADR-0094 WP-8：TTS 函数指针
	// 收进 Library 结构体后，"已加载" 状态由 libInst != nil 判定，不再有独立
	// 的 loaded bool 标志）。
	libMu.Lock()
	instBefore := libInst
	libMu.Unlock()

	if instBefore != nil {
		t.Skip("library already loaded in this process, cannot test failure path")
	}

	err := LoadLibrary("/nonexistent/path/to/libsherpa.so")
	if err == nil {
		t.Error("expected error for nonexistent library path, got nil")
	}

	// 恢复状态
	libMu.Lock()
	if instBefore == nil {
		libInst = nil
		loadErr = nil
	}
	libMu.Unlock()
}

func TestNewEngine_NotLoaded(t *testing.T) {
	libMu.Lock()
	instBefore := libInst
	libInst = nil
	libMu.Unlock()
	defer func() {
		libMu.Lock()
		libInst = instBefore
		libMu.Unlock()
	}()

	_, err := NewEngine("/some/model/dir", Options{NumThreads: 2})
	if err == nil {
		t.Error("expected error when library not loaded, got nil")
	}
}

// ── 模型必需文件 ───────────────────────────────────────────────────────────

// writeModelFiles 在 dir 下按 requiredFiles 造一份"完整"模型目录（内容为占位）。
func writeModelFiles(t *testing.T, dir string, skip string) {
	t.Helper()
	for _, rel := range requiredFiles() {
		if rel == skip {
			continue
		}
		full := filepath.Join(dir, strings.TrimSuffix(rel, "/"))
		if strings.HasSuffix(rel, "/") {
			if err := os.MkdirAll(filepath.Join(full, "en"), 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestModelMissing(t *testing.T) {
	if ModelMissing("/nonexistent/dir") == "" {
		t.Error("不存在的目录必须报缺失")
	}
	dir := t.TempDir()
	if got := ModelMissing(dir); got != "model.onnx" {
		t.Errorf("空目录应首先报缺 model.onnx，got %q", got)
	}
	writeModelFiles(t, dir, "")
	if got := ModelMissing(dir); got != "" {
		t.Errorf("齐备目录不应报缺失，got %q", got)
	}
	// 逐项缺失：espeak-ng-data / lexicon 缺任何一项引擎都会读错音或创建失败。
	for _, skip := range requiredFiles() {
		d := t.TempDir()
		writeModelFiles(t, d, skip)
		if got := ModelMissing(d); got != skip {
			t.Errorf("缺 %q 时应报该项，got %q", skip, got)
		}
	}
}

// espeak-ng-data 必须是目录：同名普通文件不算数。
func TestModelMissing_DirVsFile(t *testing.T) {
	dir := t.TempDir()
	writeModelFiles(t, dir, "espeak-ng-data/")
	if err := os.WriteFile(filepath.Join(dir, "espeak-ng-data"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ModelMissing(dir); got != "espeak-ng-data/" {
		t.Errorf("espeak-ng-data 是文件而非目录，应判缺失，got %q", got)
	}
}

// ── ttsModelMapper ─────────────────────────────────────────────────────────

// 保留归档内完整目录结构，只剥掉顶层目录（espeak-ng-data/dict/多份 lexicon+fst 都要）。
func TestTTSModelMapper_KeepsStructureStripsTopDir(t *testing.T) {
	mapper := ttsModelMapper("/models")
	cases := map[string]string{
		"kokoro-multi-lang-v1_1/model.onnx":                   "/models/model.onnx",
		"kokoro-multi-lang-v1_1/espeak-ng-data/en/rules":      "/models/espeak-ng-data/en/rules",
		"kokoro-multi-lang-v1_1/dict/pos_dict/prob_emit.utf8": "/models/dict/pos_dict/prob_emit.utf8",
		"kokoro-multi-lang-v1_1/number-zh.fst":                "/models/number-zh.fst",
		"./kokoro-multi-lang-v1_1/lexicon-zh.txt":             "/models/lexicon-zh.txt",
	}
	for in, want := range cases {
		got, ok := mapper(in)
		if !ok || got != want {
			t.Errorf("mapper(%q) = %q, %v; want %q, true", in, got, ok, want)
		}
	}
}

func TestTTSModelMapper_DropsTopLevelAndEscapes(t *testing.T) {
	mapper := ttsModelMapper("/models")
	for _, in := range []string{"loose.txt", "kokoro-multi-lang-v1_1/", "kokoro-multi-lang-v1_1"} {
		if got, ok := mapper(in); ok {
			t.Errorf("mapper(%q) 应丢弃，got %q", in, got)
		}
	}
	// 路径逃逸：无论如何都不得映射到 modelDir 之外。
	for _, in := range []string{"x/../../etc/passwd", "/etc/passwd", "a/../../../b"} {
		got, ok := mapper(in)
		if ok && !strings.HasPrefix(got, "/models/") {
			t.Errorf("mapper(%q) = %q 逃出了 modelDir", in, got)
		}
	}
}

// ── ModelDir ───────────────────────────────────────────────────────────────

func TestModelDir(t *testing.T) {
	got := ModelDir("/tts")
	if got != "/tts/model" {
		t.Errorf("got %q, want %q", got, "/tts/model")
	}
}

// 缺失资产清单与 Installed 一致。
func TestInstalled_And_MissingAssets(t *testing.T) {
	libDir, ttsDir := t.TempDir(), t.TempDir()
	if Installed(libDir, ttsDir) {
		t.Fatal("空目录不应判为已安装")
	}
	writeModelFiles(t, ModelDir(ttsDir), "")
	missing := MissingAssets(libDir, ttsDir)
	for _, a := range missing {
		if strings.Contains(a.File, "kokoro") {
			t.Error("模型已齐备，不应再列 Kokoro")
		}
	}
}
