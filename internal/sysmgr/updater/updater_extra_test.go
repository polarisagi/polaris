package updater

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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
	// 显式置空信任根：本用例的范围是"SHA-256 计算与比对是否正确"，写于发布签名
	// 存在之前。New 现在会从 releasekeys/ 快照真实信任根，一旦仓库提交了公钥，
	// 客户端即转 fail-closed 并要求 .sig，而本用例的 mock 不提供签名——
	// 于是它会以"签名缺失"失败，掩盖掉它真正要测的东西。
	// 签名相关的信任状态由 checksum_trust_test.go 专门覆盖。
	m.releaseKeys = nil
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
	m.exitFn = func(code int) { t.Error("writeWindowsUpdateScript must not exit the process (GR-10.2-003)") }
	started := ""
	m.startScriptFn = func(p string) error { started = p; return nil }

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
	if started != scriptPath {
		t.Errorf("update script not launched, started=%q", started)
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

	err := m.applyUpdate(fakeArchive, "v2.0.0")
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

func TestCheckLatest_DevBuildNoUpdate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/polarisagi/polaris/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"tag_name": "v2.5.0", "body": "n", "html_url": "u"})
	})
	m := New("dev", "", "", &http.Client{Transport: &mockTransport{handler: mux}})
	m.CheckLatest(context.Background())
	info := m.GetVersionInfo()
	if info.HasUpdate {
		t.Error("dev 构建不应有更新")
	}
	if !info.DevBuild {
		t.Error("dev 构建应标记 DevBuild")
	}
	if info.Latest != "v2.5.0" {
		t.Errorf("dev 构建仍应填充 Latest，got %q", info.Latest)
	}
}

func TestTriggerUpdate_DevBuildRejected(t *testing.T) {
	m := New("dev", "", "", http.DefaultClient)
	err := m.TriggerUpdate(context.Background(), "v2.5.0")
	if err == nil {
		t.Fatal("dev 构建应拒绝在线更新")
	}
	if !apperr.IsCode(err, apperr.CodeInvalidInput) {
		t.Errorf("期望 CodeInvalidInput，got %v", err)
	}
	if m.GetVersionInfo().UpdateStatus != StatusIdle {
		t.Error("拒绝后状态应保持 idle")
	}
}

// writeTestArchive 构造含 polaris 二进制与 lib/libx.so 的 tar.gz。
func writeTestArchive(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"polaris": "new binary", "lib/libx.so": "new lib"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
}

func setupApplyUpdate(t *testing.T) (m *Manager, exe, archive string) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix 替换路径")
	}
	dir := t.TempDir()
	exe = filepath.Join(dir, "polaris")
	os.WriteFile(exe, []byte("old binary"), 0o755)
	os.MkdirAll(filepath.Join(dir, "lib"), 0o755)
	os.WriteFile(filepath.Join(dir, "lib", "libx.so"), []byte("old lib"), 0o644)
	archive = filepath.Join(dir, "a.tar.gz")
	writeTestArchive(t, archive)
	m = New("v1.0.0", "", "", http.DefaultClient)
	m.executableFn = func() (string, error) { return exe, nil }
	return m, exe, archive
}

func TestApplyUpdate_ProbeMismatchKeepsBinary(t *testing.T) {
	m, exe, archive := setupApplyUpdate(t)
	m.probeBinaryFn = func(ctx context.Context, p string) (string, error) { return "polaris v9.9.9\n", nil }
	if err := m.applyUpdate(archive, "v2.0.0"); err == nil {
		t.Fatal("预检版本不符应返回错误")
	}
	if b, _ := os.ReadFile(exe); string(b) != "old binary" {
		t.Errorf("二进制不应被替换，got %q", b)
	}
	for _, p := range []string{exe + ".new", filepath.Join(filepath.Dir(exe), "lib.new"), exe + ".prev"} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s 应不存在", p)
		}
	}
}

func TestApplyUpdate_ProbeOKCreatesPrev(t *testing.T) {
	m, exe, archive := setupApplyUpdate(t)
	m.probeBinaryFn = func(ctx context.Context, p string) (string, error) {
		return "polaris v2.0.0\n", nil
	}
	if err := m.applyUpdate(archive, "v2.0.0"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new binary" {
		t.Errorf("二进制应已替换，got %q", b)
	}
	if b, _ := os.ReadFile(exe + ".prev"); string(b) != "old binary" {
		t.Errorf(".prev 应为旧二进制，got %q", b)
	}
	b, _ := os.ReadFile(filepath.Join(filepath.Dir(exe), "lib.prev", "libx.so"))
	if !strings.Contains(string(b), "old lib") {
		t.Errorf("lib.prev 应保留旧库，got %q", b)
	}
}
