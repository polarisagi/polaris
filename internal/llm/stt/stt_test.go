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

// ── 清单 ──────────────────────────────────────────────────────────────────────

// 当前平台必须有库清单项（否则语音在该平台永远 unsupported，应由此测试提前暴露）。
func TestLibAssetForHost(t *testing.T) {
	a, err := LibAssetForHost()
	if err != nil {
		t.Skipf("当前平台 %s/%s 无库清单项: %v", runtime.GOOS, runtime.GOARCH, err)
	}
	if !strings.Contains(a.File, "v"+SherpaABIVersion) {
		t.Errorf("库资产未绑定 ABI 版本: %s", a.File)
	}
}

// 缺失资产清单：空目录应列出库+模型+标点三项；补齐后为空。
func TestMissingAssets_AndInstalled(t *testing.T) {
	dir := t.TempDir()
	if _, err := LibAssetForHost(); err != nil {
		t.Skip("平台无库清单项")
	}
	if Installed(dir) {
		t.Fatal("空目录不应判为已安装")
	}
	if got := len(MissingAssets(dir)); got != 3 {
		t.Errorf("空目录应缺 3 项（库/模型/标点），got %d", got)
	}
	mustWrite := func(rel string) {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(LibName())
	mustWrite("model/model.onnx")
	mustWrite("model/tokens.txt")
	if Installed(dir) {
		t.Error("缺标点模型不应判为已安装")
	}
	mustWrite("punct_model/model.onnx")
	if !Installed(dir) {
		t.Errorf("三项齐备应判为已安装，missing=%v", MissingAssets(dir))
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
	err := EnsureAssets(context.Background(), t.TempDir(), nil, "9.9.9", nil)
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
