package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func TestManager_CheckLatest(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/polarisagi/polaris/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"tag_name": "v1.7.6",
			"body":     "Release notes",
			"html_url": "https://example.com/release/v1.7.6",
		})
	})

	// A mock transport
	client := &http.Client{}
	client.Transport = &mockTransport{
		handler: mux,
	}

	m := New("v1.0.0", "abc", "2024", client)
	m.CheckLatest(context.Background())

	info := m.GetVersionInfo()
	if info.Latest != "v1.7.6" {
		t.Errorf("expected v1.7.6, got %s", info.Latest)
	}
	if !info.HasUpdate {
		t.Errorf("expected has update to be true")
	}
}

type mockTransport struct {
	handler http.Handler
}

func (m *mockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if m.handler != nil {
		rec := httptest.NewRecorder()
		m.handler.ServeHTTP(rec, req)
		return rec.Result(), nil
	}
	return nil, apperr.New(apperr.CodeInternal, "unhandled request")
}

func TestManager_StartBackgroundCheck(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/polarisagi/polaris/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"tag_name": "v1.2.4",
			"body":     "Release notes",
			"html_url": "https://example.com/release/v1.2.4",
		})
	})

	client := &http.Client{}
	client.Transport = &mockTransport{
		handler: mux,
	}

	m := New("v1.0.0", "abc", "2024", client)
	ctx, cancel := context.WithCancel(context.Background())
	m.StartBackgroundCheck(ctx, 10*time.Millisecond)

	// Since StartBackgroundCheck waits for 30s first, this test won't trigger easily
	// We just ensure it doesn't panic.
	time.Sleep(10 * time.Millisecond)
	cancel()
}

func TestManager_TriggerUpdate_Errors(t *testing.T) {
	m := New("v1.0.0", "abc", "2024", nil)
	err := m.TriggerUpdate(context.Background(), "")
	if err == nil {
		t.Error("expected error for empty version")
	}

	m.setStatus(StatusDownloading)
	err = m.TriggerUpdate(context.Background(), "v1.7.6")
	if err == nil {
		t.Error("expected error for concurrent update")
	}
}

func TestManager_VerifyChecksum(t *testing.T) {
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "polaris-test.tar.gz")

	content := []byte("fake archive content")
	os.WriteFile(archivePath, content, 0644)

	h := sha256.New()
	h.Write(content)
	checksum := hex.EncodeToString(h.Sum(nil))

	mux := http.NewServeMux()
	checksumsURL := "/polarisagi/polaris/releases/download/v1.7.6/polaris-test.tar.gz.sha256"
	mux.HandleFunc(checksumsURL, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  polaris-test.tar.gz\n", checksum)
	})

	client := &http.Client{}
	client.Transport = &mockTransport{
		handler: mux,
	}

	m := New("v1.0.0", "abc", "2024", client)
	err := m.verifyChecksum(context.Background(), "v1.7.6", "polaris-test.tar.gz", archivePath)
	if err != nil {
		t.Errorf("expected success, got %v", err)
	}
}

func TestReplaceUnixLibs(t *testing.T) {
	tmpDir := t.TempDir()
	newLibDir := filepath.Join(tmpDir, "lib.new")
	targetLibDir := filepath.Join(tmpDir, "lib")

	os.MkdirAll(newLibDir, 0755)
	os.WriteFile(filepath.Join(newLibDir, "test.so"), []byte("test"), 0644)

	err := replaceUnixLibs(newLibDir, targetLibDir)
	if err != nil {
		t.Errorf("replaceUnixLibs failed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(targetLibDir, "test.so")); err != nil {
		t.Errorf("file not moved: %v", err)
	}
	if _, err := os.Stat(newLibDir); !os.IsNotExist(err) {
		t.Errorf("newLibDir not removed")
	}
}

func TestWriteWindowsUpdateScript(t *testing.T) {
	m := &Manager{}
	m.exitFn = func(code int) {}

	tmpDir := t.TempDir()
	exePath := filepath.Join(tmpDir, "polaris.exe")
	newBinPath := filepath.Join(tmpDir, "polaris.exe.new")
	targetLibDir := filepath.Join(tmpDir, "lib")
	newLibDir := filepath.Join(tmpDir, "lib.new")

	err := m.writeWindowsUpdateScript(exePath, newBinPath, targetLibDir, newLibDir)
	if err != nil {
		t.Errorf("writeWindowsUpdateScript failed: %v", err)
	}

	scriptPath := exePath + ".update.bat"
	if _, err := os.Stat(scriptPath); err != nil {
		t.Errorf("script not written: %v", err)
	}

	time.Sleep(300 * time.Millisecond)
}

func TestApplyUpdate(t *testing.T) {
	tmpDir := t.TempDir()
	fakeExe := filepath.Join(tmpDir, "polaris")
	os.WriteFile(fakeExe, []byte("old binary"), 0755)

	m := New("v1.0.0", "abc", "2024", http.DefaultClient)
	m.executableFn = func() (string, error) {
		return fakeExe, nil
	}

	// Since we mock os.Executable, we can create a fake archive file.
	// However, we don't have a valid zip/tar.gz here to test extractFiles fully via applyUpdate.
	// But we can trigger extractFiles error path.
	fakeArchive := filepath.Join(tmpDir, "fake.zip")
	os.WriteFile(fakeArchive, []byte("not a zip"), 0644)

	err := m.applyUpdate(fakeArchive)
	if err == nil {
		t.Errorf("expected error for invalid archive")
	}
}

func TestDefaultRestart(t *testing.T) {
	osExitCalled := false
	m := &Manager{
		exitFn: func(code int) {
			osExitCalled = true
		},
	}

	m.defaultRestart("/fake/path")
	if !osExitCalled {
		t.Errorf("expected osExit to be called")
	}
}

func TestExtractFiles(t *testing.T) {
	tmpDir := t.TempDir()

	err := extractFiles(filepath.Join(tmpDir, "nonexistent.tar.gz"), filepath.Join(tmpDir, "bin"), filepath.Join(tmpDir, "lib"))
	if err == nil {
		t.Errorf("expected error for nonexistent file")
	}
}

func TestManager_doUpdate_ErrorPath(t *testing.T) {
	m := New("v1.0.0", "abc", "2024", http.DefaultClient)
	// Triggers download error since we pass a non-existent version

	client := &http.Client{
		Transport: &mockTransport{
			handler: nil,
		},
	}
	m.client = client

	ctx := context.Background()
	m.doUpdate(ctx, "v9.9.9")

	if m.GetVersionInfo().UpdateStatus != StatusError {
		t.Errorf("expected status error, got %v", m.GetVersionInfo().UpdateStatus)
	}
}
