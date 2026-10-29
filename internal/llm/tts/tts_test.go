package tts

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polarisagi/polaris/internal/llm/audioassets"
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
	// 逐项缺失：词典/dict 缺任何一项引擎都会读错音或创建失败。
	for _, skip := range requiredFiles() {
		d := t.TempDir()
		writeModelFiles(t, d, skip)
		if got := ModelMissing(d); got != skip {
			t.Errorf("缺 %q 时应报该项，got %q", skip, got)
		}
	}
}

// Melo 的 model.int8.onnx 是 133 字节占位文件：既不是必需文件，也不会被落盘。
func TestMelo_Int8PlaceholderNotRequired(t *testing.T) {
	for _, rel := range requiredFiles() {
		if strings.Contains(rel, "int8") {
			t.Errorf("必需文件不得含 int8 占位：%s", rel)
		}
	}
	if _, ok := ttsModelMapper("/m")("vits-melo-tts-zh_en/model.int8.onnx"); ok {
		t.Error("Melo 的 model.int8.onnx 应被 mapper 丢弃")
	}
}

// dict 必须是目录：同名普通文件不算数。
func TestModelMissing_DirVsFile(t *testing.T) {
	dir := t.TempDir()
	writeModelFiles(t, dir, "dict/")
	if err := os.WriteFile(filepath.Join(dir, "dict"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ModelMissing(dir); got != "dict/" {
		t.Errorf("dict 是文件而非目录，应判缺失，got %q", got)
	}
}

// ── ttsModelMapper ─────────────────────────────────────────────────────────

// 保留归档内完整目录结构，只剥掉顶层目录（dict/lexicon/fst 都要）。
func TestTTSModelMapper_KeepsStructureStripsTopDir(t *testing.T) {
	mapper := ttsModelMapper("/models")
	cases := map[string]string{
		"vits-melo-tts-zh_en/model.onnx":           "/models/model.onnx",
		"vits-melo-tts-zh_en/dict/jieba.dict.utf8": "/models/dict/jieba.dict.utf8",
		"vits-melo-tts-zh_en/number.fst":           "/models/number.fst",
		"./vits-melo-tts-zh_en/lexicon.txt":        "/models/lexicon.txt",
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
	for _, in := range []string{"loose.txt", "vits-melo-tts-zh_en/", "vits-melo-tts-zh_en"} {
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
	if got := ModelDir("/tts"); got != "/tts/melo" {
		t.Errorf("got %q, want /tts/melo", got)
	}
}

// 缺失资产清单与 Installed 一致。
func TestInstalled_And_MissingAssets(t *testing.T) {
	libDir, ttsDir := t.TempDir(), t.TempDir()
	if Installed(libDir, ttsDir) {
		t.Fatal("空目录不应判为已安装")
	}
	writeModelFiles(t, ModelDir(ttsDir), "")
	for _, a := range MissingAssets(libDir, ttsDir) {
		if a.Kind == audioassets.KindTTSModel {
			t.Errorf("模型已齐备，不应再列 %s", a.File)
		}
	}
}

// ── SplitSentences ─────────────────────────────────────────────────────────

func TestSplitSentences(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"你好。", []string{"你好。"}},
		{"好的，我已经帮你查过了。另外，请打开！真的吗？是的；好。", []string{"好的，我已经帮你查过了。", "另外，请打开！", "真的吗？", "是的；", "好。"}},
		{"line one\nline two", []string{"line one", "line two"}},
		{"a! b? c; d", []string{"a!", "b?", "c;", "d"}},
		// 连续标点与收尾引号归入本句，不产生孤立的标点句。
		{"真的？！“好。”再见", []string{"真的？！", "“好。”", "再见"}},
		// 逗号与 ASCII 句点不切（切逗号会复现吞字，切句点会破坏小数/域名）。
		{"版本 1.13.2，请升级", []string{"版本 1.13.2，请升级"}},
		// 无实义内容的碎片并入上一句 / 被丢弃。
		{"好。。。 ！", []string{"好。。。 ！"}},
		{"。好", []string{"。好"}},
		{"   \n  ", nil},
		{"", nil},
		{"没有标点的一句话", []string{"没有标点的一句话"}},
	}
	for _, c := range cases {
		got := SplitSentences(c.in)
		if len(got) != len(c.want) {
			t.Errorf("SplitSentences(%q) = %q, want %q", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("SplitSentences(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}
