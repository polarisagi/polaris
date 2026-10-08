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

	_, err := NewEngine("/some/model/dir", Options{Model: ModelMelo, NumThreads: 2})
	if err == nil {
		t.Error("expected error when library not loaded, got nil")
	}
}

// ── 模型必需文件 ───────────────────────────────────────────────────────────

var allModels = []Model{ModelMelo, ModelMatcha}

// writeModelFiles 在 dir 下按 requiredFiles 造一份"完整"模型目录（内容为占位）。
func writeModelFiles(t *testing.T, dir string, m Model, skip string) {
	t.Helper()
	for _, rel := range requiredFiles(m) {
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
	for _, m := range allModels {
		if ModelMissing("/nonexistent/dir", m) == "" {
			t.Errorf("%s: 不存在的目录必须报缺失", m)
		}
		dir := t.TempDir()
		if got := ModelMissing(dir, m); got != requiredFiles(m)[0] {
			t.Errorf("%s: 空目录应首先报缺 %s，got %q", m, requiredFiles(m)[0], got)
		}
		writeModelFiles(t, dir, m, "")
		if got := ModelMissing(dir, m); got != "" {
			t.Errorf("%s: 齐备目录不应报缺失，got %q", m, got)
		}
		// 逐项缺失：词典/声码器/espeak-ng-data 缺任何一项引擎都会读错音或创建失败。
		for _, skip := range requiredFiles(m) {
			d := t.TempDir()
			writeModelFiles(t, d, m, skip)
			if got := ModelMissing(d, m); got != skip {
				t.Errorf("%s: 缺 %q 时应报该项，got %q", m, skip, got)
			}
		}
	}
}

// Melo 的 model.int8.onnx 是 133 字节占位文件：既不是必需文件，也不会被落盘。
func TestMelo_Int8PlaceholderNotRequired(t *testing.T) {
	for _, rel := range requiredFiles(ModelMelo) {
		if strings.Contains(rel, "int8") {
			t.Errorf("Melo 必需文件不得含 int8 占位：%s", rel)
		}
	}
	if _, ok := ttsModelMapper("/m", ModelMelo)("vits-melo-tts-zh_en/model.int8.onnx"); ok {
		t.Error("Melo 的 model.int8.onnx 应被 mapper 丢弃")
	}
}

// Matcha 声码器是独立单文件：归档齐备但声码器缺失时，只应补声码器这一项。
func TestMissingAssets_MatchaVocoderSeparate(t *testing.T) {
	libDir, ttsDir := t.TempDir(), t.TempDir()
	writeModelFiles(t, ModelDir(ttsDir, ModelMatcha), ModelMatcha, MatchaVocoderFile)
	names := make([]string, 0, 3)
	for _, a := range MissingAssets(libDir, ttsDir, ModelMatcha) {
		names = append(names, a.File)
	}
	if !strings.Contains(strings.Join(names, ","), "vocos-16khz-univ.onnx") || strings.Contains(strings.Join(names, ","), "matcha-icefall") {
		t.Errorf("只应缺声码器，got %v", names)
	}
}

// espeak-ng-data 必须是目录：同名普通文件不算数。
func TestModelMissing_DirVsFile(t *testing.T) {
	dir := t.TempDir()
	writeModelFiles(t, dir, ModelMatcha, "espeak-ng-data/")
	if err := os.WriteFile(filepath.Join(dir, "espeak-ng-data"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ModelMissing(dir, ModelMatcha); got != "espeak-ng-data/" {
		t.Errorf("espeak-ng-data 是文件而非目录，应判缺失，got %q", got)
	}
}

// ── ttsModelMapper ─────────────────────────────────────────────────────────

// 保留归档内完整目录结构，只剥掉顶层目录（espeak-ng-data/dict/lexicon+fst 都要）。
func TestTTSModelMapper_KeepsStructureStripsTopDir(t *testing.T) {
	mapper := ttsModelMapper("/models", ModelMatcha)
	cases := map[string]string{
		"matcha-icefall-zh-en/model-steps-3.onnx":      "/models/model-steps-3.onnx",
		"matcha-icefall-zh-en/espeak-ng-data/en/rules": "/models/espeak-ng-data/en/rules",
		"matcha-icefall-zh-en/number-zh.fst":           "/models/number-zh.fst",
		"./matcha-icefall-zh-en/lexicon.txt":           "/models/lexicon.txt",
	}
	for in, want := range cases {
		got, ok := mapper(in)
		if !ok || got != want {
			t.Errorf("mapper(%q) = %q, %v; want %q, true", in, got, ok, want)
		}
	}
	got, ok := ttsModelMapper("/models", ModelMelo)("vits-melo-tts-zh_en/dict/jieba.dict.utf8")
	if !ok || got != "/models/dict/jieba.dict.utf8" {
		t.Errorf("melo dict 映射错误：%q %v", got, ok)
	}
}

func TestTTSModelMapper_DropsTopLevelAndEscapes(t *testing.T) {
	mapper := ttsModelMapper("/models", ModelMelo)
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

// ── ModelDir / ParseModel ──────────────────────────────────────────────────

func TestModelDir(t *testing.T) {
	if got := ModelDir("/tts", ModelMelo); got != "/tts/melo" {
		t.Errorf("got %q, want /tts/melo", got)
	}
	if got := ModelDir("/tts", ModelMatcha); got != "/tts/matcha" {
		t.Errorf("got %q, want /tts/matcha", got)
	}
}

func TestParseModel(t *testing.T) {
	for _, ok := range []string{"melo", "matcha"} {
		if _, err := ParseModel(ok); err != nil {
			t.Errorf("%s 应合法：%v", ok, err)
		}
	}
	for _, bad := range []string{"", "kokoro", "Melo", "auto"} {
		if _, err := ParseModel(bad); err == nil {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
}

// 缺失资产清单与 Installed 一致。
func TestInstalled_And_MissingAssets(t *testing.T) {
	for _, m := range allModels {
		libDir, ttsDir := t.TempDir(), t.TempDir()
		if Installed(libDir, ttsDir, m) {
			t.Fatalf("%s: 空目录不应判为已安装", m)
		}
		writeModelFiles(t, ModelDir(ttsDir, m), m, "")
		for _, a := range MissingAssets(libDir, ttsDir, m) {
			if a.Kind == audioassets.KindTTSModel || a.Kind == audioassets.KindTTSFile {
				t.Errorf("%s: 模型已齐备，不应再列 %s", m, a.File)
			}
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
