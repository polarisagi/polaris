package stt

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ── LibName ───────────────────────────────────────────────────────────────────

func TestLibName_Platform(t *testing.T) {
	name := LibName()
	if name == "" {
		t.Fatal("LibName should not be empty")
	}
	switch runtime.GOOS {
	case "darwin":
		if name != "libsherpa-onnx-c-api.dylib" {
			t.Errorf("macOS expected .dylib, got %q", name)
		}
	default:
		if name != "libsherpa-onnx-c-api.so" {
			t.Errorf("Linux expected .so, got %q", name)
		}
	}
}

// ── libDownloadURL ────────────────────────────────────────────────────────────

// 资产名是对 GitHub API 资产清单实测的结果（v1.13.2）；此表若与真实清单漂移，
// 由 make audio-nettest 的 HEAD 校验兜底（单测不联网，只钉死拼接逻辑）。
func TestLibDownloadURL_ExactNames(t *testing.T) {
	const base = "https://github.com/k2-fsa/sherpa-onnx/releases/download/v1.13.2/"
	cases := []struct{ goos, goarch, file string }{
		{"darwin", "arm64", "sherpa-onnx-v1.13.2-osx-arm64-shared-lib.tar.bz2"},
		{"darwin", "amd64", "sherpa-onnx-v1.13.2-osx-x64-shared-lib.tar.bz2"},
		{"linux", "amd64", "sherpa-onnx-v1.13.2-linux-x64-shared-lib.tar.bz2"},
		{"linux", "arm64", "sherpa-onnx-v1.13.2-linux-aarch64-shared-cpu-lib.tar.bz2"},
		{"windows", "amd64", "sherpa-onnx-v1.13.2-win-x64-shared-MT-Release-lib.tar.bz2"},
	}
	for _, c := range cases {
		got, err := libDownloadURL(c.goos, c.goarch, "1.13.2")
		if err != nil {
			t.Fatalf("%s/%s: %v", c.goos, c.goarch, err)
		}
		if want := base + c.file; got != want {
			t.Errorf("%s/%s:\n got  %s\n want %s", c.goos, c.goarch, got, want)
		}
	}
	if _, err := libDownloadURL("plan9", "amd64", "1.13.2"); err == nil {
		t.Error("unsupported platform should return error")
	}
	if _, err := libDownloadURL("linux", "amd64", ""); err == nil {
		t.Error("empty version should return error")
	}
}

// ABI 钉死：配置版本与 SherpaABIVersion 不同必须报错，空串与相同版本放行。
func TestResolveSherpaVersion(t *testing.T) {
	for _, v := range []string{"", SherpaABIVersion} {
		got, err := ResolveSherpaVersion(v)
		if err != nil || got != SherpaABIVersion {
			t.Errorf("ResolveSherpaVersion(%q) = %q, %v; want %q, nil", v, got, err, SherpaABIVersion)
		}
	}
	if _, err := ResolveSherpaVersion("1.12.0"); err == nil {
		t.Error("mismatched version should be rejected")
	}
}

// EnsureAssets 对版本不一致的配置必须在任何下载前就报错。
func TestEnsureAssets_RejectsABIMismatch(t *testing.T) {
	err := EnsureAssets(context.Background(), t.TempDir(), nil, "9.9.9", "http://invalid/m.tar.bz2", "", nil)
	if err == nil {
		t.Fatal("expected ABI mismatch error")
	}
}

// ── mapper ────────────────────────────────────────────────────────────────────

// int8 归档内是 model.int8.onnx，必须映射到引擎唯一认的 model.onnx（S3 回归）。
func TestSTTModelMapper_Int8(t *testing.T) {
	m := sttModelMapper("/m")
	cases := map[string]string{
		"sherpa-onnx-sense-voice-int8/model.int8.onnx": "/m/model.onnx",
		"sherpa-onnx-sense-voice/model.onnx":           "/m/model.onnx",
		"sherpa-onnx-sense-voice-int8/tokens.txt":      "/m/tokens.txt",
	}
	for in, want := range cases {
		got, ok := m(in)
		if !ok || got != want {
			t.Errorf("mapper(%q) = %q, %v; want %q, true", in, got, ok, want)
		}
	}
	if _, ok := m("x/README.md"); ok {
		t.Error("README.md should be skipped")
	}
}

func TestPunctModelMapper(t *testing.T) {
	m := punctModelMapper("/p")
	cases := map[string]string{
		"punct-int8/model.int8.onnx": "/p/model.onnx",
		"punct/model.onnx":           "/p/model.onnx",
		"punct-int8/tokens.json":     "/p/tokens.json",
	}
	for in, want := range cases {
		got, ok := m(in)
		if !ok || got != want {
			t.Errorf("mapper(%q) = %q, %v; want %q, true", in, got, ok, want)
		}
	}
	if _, ok := m("punct/tokens.txt"); ok {
		t.Error("punct mapper must not take tokens.txt (STT 专用)")
	}
}

// 归档解压成功但缺少必需文件（S3：只抽出 tokens.txt）→ 必须报明确错误，而非判成功。
func TestRequireFiles_MissingReportsArchive(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "tokens.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := requireFiles(dir, "https://h/asr-models/foo-int8.tar.bz2", "model.onnx", "tokens.txt")
	if err == nil {
		t.Fatal("expected error for missing model.onnx")
	}
	if !strings.Contains(err.Error(), "foo-int8.tar.bz2") || !strings.Contains(err.Error(), "model.onnx") {
		t.Errorf("error should name archive and file, got: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "model.onnx"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := requireFiles(dir, "u", "model.onnx", "tokens.txt"); err != nil {
		t.Errorf("all present should pass: %v", err)
	}
}

// ── ModelDir / PunctModelDir ──────────────────────────────────────────────────

func TestModelDir(t *testing.T) {
	dir := ModelDir("/var/polaris/stt")
	if dir != "/var/polaris/stt/model" {
		t.Errorf("expected /var/polaris/stt/model, got %q", dir)
	}
}

func TestPunctModelDir(t *testing.T) {
	dir := PunctModelDir("/var/polaris/stt")
	if dir != "/var/polaris/stt/punct_model" {
		t.Errorf("expected /var/polaris/stt/punct_model, got %q", dir)
	}
}

// ── modelFilesPresent ─────────────────────────────────────────────────────────

func TestModelFilesPresent_Missing(t *testing.T) {
	if modelFilesPresent("/nonexistent/model/dir") {
		t.Error("non-existent dir should return false")
	}
}

func TestModelFilesPresent_Partial(t *testing.T) {
	tmp := t.TempDir()
	// 只写 model.onnx，缺 tokens.txt
	os.WriteFile(filepath.Join(tmp, "model.onnx"), []byte("dummy"), 0o644)
	if modelFilesPresent(tmp) {
		t.Error("partial files should return false")
	}
}

func TestModelFilesPresent_Complete(t *testing.T) {
	tmp := t.TempDir()
	for _, f := range []string{"model.onnx", "tokens.txt"} {
		os.WriteFile(filepath.Join(tmp, f), []byte("dummy"), 0o644)
	}
	if !modelFilesPresent(tmp) {
		t.Error("all required files present should return true")
	}
}

// ── LoadLibrary ───────────────────────────────────────────────────────────────

func TestLoadLibrary_NonExistentPath(t *testing.T) {
	// 确保重置全局状态（避免已加载状态干扰）
	libMu.Lock()
	wasLoaded := libInst != nil
	libMu.Unlock()

	if wasLoaded {
		t.Skip("library already loaded in this process; skipping LoadLibrary error test")
	}

	err := LoadLibrary("/nonexistent/libsherpa-onnx-c-api.so")
	if err == nil {
		t.Fatal("non-existent library path should return error")
	}
}

// ── NewEngine (library not loaded) ───────────────────────────────────────────

func TestNewEngine_LibraryNotLoaded(t *testing.T) {
	libMu.Lock()
	wasLoaded := libInst != nil
	savedLoaded := libInst
	if wasLoaded {
		libInst = nil // 临时模拟未加载状态
	}
	libMu.Unlock()

	defer func() {
		libMu.Lock()
		libInst = savedLoaded
		libMu.Unlock()
	}()

	e, err := NewEngine("/tmp/model", "", "zh", 1, false)
	if err == nil {
		t.Fatal("NewEngine with unloaded library must return an error (no empty-shell engine)")
	}
	if e != nil {
		t.Fatal("expected nil Engine on error")
	}
}

// ── Engine.Transcribe (no real library) ──────────────────────────────────────

func TestTranscribe_UninitializedErrors(t *testing.T) {
	e := &Engine{recognizer: nil}
	res, err := e.Transcribe([]float32{0.1, 0.2}, 16000)
	if err == nil {
		t.Fatal("uninitialized engine must return error, never fake text")
	}
	if res.Text != "" {
		t.Errorf("no text expected on error, got %q", res.Text)
	}
}

// ── parseCString ──────────────────────────────────────────────────────────────

func TestParseCString_Zero(t *testing.T) {
	s := parseCString(0)
	if s != "" {
		t.Errorf("ptr=0 should return empty string, got %q", s)
	}
}
