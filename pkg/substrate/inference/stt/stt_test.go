package stt

import (
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

func TestLibDownloadURL_SupportedPlatforms(t *testing.T) {
	url, err := libDownloadURL("1.13.2")
	switch {
	case runtime.GOOS == "darwin", runtime.GOOS == "linux":
		if err != nil {
			t.Fatalf("supported platform should not error: %v", err)
		}
		if !strings.Contains(url, "1.13.2") {
			t.Errorf("URL should contain version, got %q", url)
		}
		if !strings.HasSuffix(url, ".tar.bz2") {
			t.Errorf("URL should end with .tar.bz2, got %q", url)
		}
	default:
		if err == nil {
			t.Error("unsupported platform should return error")
		}
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
	for _, f := range modelRequiredFiles {
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
	wasLoaded := loaded
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
	wasLoaded := loaded
	savedLoaded := loaded
	if wasLoaded {
		loaded = false // 临时模拟未加载状态
	}
	libMu.Unlock()

	defer func() {
		libMu.Lock()
		loaded = savedLoaded
		libMu.Unlock()
	}()

	e, err := NewEngine("/tmp/model", "")
	if err != nil {
		t.Fatalf("NewEngine with unloaded library should not error: %v", err)
	}
	if e == nil {
		t.Fatal("expected non-nil Engine")
	}
}

// ── Engine.Transcribe (no real library) ──────────────────────────────────────

func TestTranscribe_MockFallback(t *testing.T) {
	// recognizer=nil → mock fallback
	e := &Engine{recognizer: nil}
	text, err := e.Transcribe([]float32{0.1, 0.2}, 16000)
	if err != nil {
		t.Fatalf("mock fallback should not error: %v", err)
	}
	if text == "" {
		t.Error("mock fallback should return non-empty text")
	}
}

// ── parseCString ──────────────────────────────────────────────────────────────

func TestParseCString_Zero(t *testing.T) {
	s := parseCString(0)
	if s != "" {
		t.Errorf("ptr=0 should return empty string, got %q", s)
	}
}
