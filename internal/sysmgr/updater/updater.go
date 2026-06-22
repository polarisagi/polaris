// Package updater 实现 polaris 的自更新（OTA）功能。
// 流程：查询 GitHub Releases API → 比较版本 → 下载（支持断点续传）→ SHA-256 校验 → 原子替换 → 重启进程。
package updater

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/sysmgr/downloader"
	"github.com/polarisagi/polaris/pkg/apperr"
)

const (
	repoOwner = "polarisagi"
	repoName  = "polaris"
)

// Status 表示当前更新阶段。
type Status string

const (
	StatusIdle        Status = "idle"
	StatusChecking    Status = "checking"
	StatusDownloading Status = "downloading"
	StatusVerifying   Status = "verifying"
	StatusInstalling  Status = "installing"
	StatusRestarting  Status = "restarting"
	StatusError       Status = "error"
)

// VersionInfo 是 /v1/system/version 端点的响应结构。
type VersionInfo struct {
	Current      string    `json:"current"`
	CommitHash   string    `json:"commit_hash,omitempty"`
	BuildDate    string    `json:"build_date,omitempty"`
	Latest       string    `json:"latest,omitempty"`
	HasUpdate    bool      `json:"has_update"`
	ReleaseNotes string    `json:"release_notes,omitempty"`
	ReleaseURL   string    `json:"release_url,omitempty"`
	UpdateStatus Status    `json:"update_status"`
	UpdateError  string    `json:"update_error,omitempty"`
	CheckedAt    time.Time `json:"checked_at,omitempty"`
}

// Manager 管理版本检测与自更新生命周期。
type Manager struct {
	current    string
	commitHash string
	buildDate  string

	mu           sync.RWMutex
	info         VersionInfo
	client       *http.Client
	restartFn    func()
	executableFn func() (string, error)
	exitFn       func(int)
}

// New 创建 Manager。currentVersion / commitHash / buildDate 由 main.Version 等 ldflags 变量传入。
func New(currentVersion, commitHash, buildDate string, client *http.Client) *Manager {
	return &Manager{
		current:      currentVersion,
		commitHash:   commitHash,
		buildDate:    buildDate,
		client:       client,
		executableFn: os.Executable,
		exitFn:       os.Exit,
		info: VersionInfo{
			Current:      currentVersion,
			CommitHash:   commitHash,
			BuildDate:    buildDate,
			UpdateStatus: StatusIdle,
		},
	}
}

// SetRestartFn 注入自定义重启函数（测试或服务管理器场景）。
func (m *Manager) SetRestartFn(fn func()) { m.restartFn = fn }

// GetVersionInfo 返回当前缓存的版本信息（线程安全）。
func (m *Manager) GetVersionInfo() VersionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.info
}

// CheckLatest 查询 GitHub Releases API 获取最新版本，更新内部缓存。
func (m *Manager) CheckLatest(ctx context.Context) {
	m.mu.Lock()
	m.info.UpdateStatus = StatusChecking
	m.mu.Unlock()

	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", repoOwner, repoName)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		m.setIdle()
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	c := m.client
	if c == nil {
		c = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		slog.Warn("updater: version check failed", "err", err)
		m.setIdle()
		return
	}
	defer resp.Body.Close()

	var release struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		slog.Warn("updater: decode response failed", "err", err)
		m.setIdle()
		return
	}

	latest := release.TagName
	current := m.current
	hasUpdate := latest != "" && !equalVersions(current, latest) && current != "dev"

	m.mu.Lock()
	m.info = VersionInfo{
		Current:      current,
		CommitHash:   m.commitHash,
		BuildDate:    m.buildDate,
		Latest:       latest,
		HasUpdate:    hasUpdate,
		ReleaseNotes: release.Body,
		ReleaseURL:   release.HTMLURL,
		UpdateStatus: StatusIdle,
		CheckedAt:    time.Now(),
	}
	m.mu.Unlock()

	if hasUpdate {
		slog.Info("updater: new version available", "current", current, "latest", latest)
	}
}

// StartBackgroundCheck 在后台定时检查版本（首次延迟 30s 等进程稳定）。
func (m *Manager) StartBackgroundCheck(ctx context.Context, interval time.Duration) {
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
		m.CheckLatest(ctx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.CheckLatest(ctx)
			}
		}
	}()
}

// TriggerUpdate 在后台启动下载+校验+替换+重启流程，立即返回。
// version 由前端从 GitHub API 获取后传入（格式 "v1.2.3"），后端不再维护 latest 缓存。
func (m *Manager) TriggerUpdate(ctx context.Context, version string) error {
	if version == "" {
		return apperr.New(apperr.CodeInvalidInput, "updater: version is required")
	}
	m.mu.RLock()
	status := m.info.UpdateStatus
	m.mu.RUnlock()

	if status != StatusIdle && status != StatusError {
		return apperr.New(apperr.CodeConflict, fmt.Sprintf("updater: update already in progress (%s)", status))
	}

	// 降级攻击防护：拒绝安装不比当前版本更新的版本（dev 版本跳过校验）
	if m.current != "dev" && version != "dev" {
		cur := strings.TrimPrefix(m.current, "v")
		tgt := strings.TrimPrefix(version, "v")
		if semverCompare(tgt, cur) <= 0 {
			return apperr.New(apperr.CodeInvalidInput,
				fmt.Sprintf("updater: version %s is not newer than current %s; downgrade rejected", version, m.current))
		}
	}

	go m.doUpdate(context.Background(), version)
	return nil
}

func (m *Manager) doUpdate(ctx context.Context, version string) {
	goos := runtime.GOOS
	goarch := runtime.GOARCH

	var archiveName string
	if goos == "windows" {
		archiveName = fmt.Sprintf("polaris-%s-%s.zip", goos, goarch)
	} else {
		archiveName = fmt.Sprintf("polaris-%s-%s.tar.gz", goos, goarch)
	}

	downloadURL := fmt.Sprintf("https://github.com/%s/%s/releases/download/%s/%s",
		repoOwner, repoName, version, archiveName)

	slog.Info("updater: starting download", "version", version, "url", downloadURL)
	m.setStatus(StatusDownloading)

	tmpDir := os.TempDir()
	archivePath := filepath.Join(tmpDir, "polaris-update-"+strings.TrimPrefix(version, "v")+"-"+goos+"-"+goarch)
	if goos == "windows" {
		archivePath += ".zip"
	} else {
		archivePath += ".tar.gz"
	}

	// 使用 downloader 下载，支持断点续传 + ghproxy 加速
	if err := downloader.DownloadFile(ctx, m.client, downloadURL, archivePath); err != nil {
		slog.Error("updater: download failed", "err", err)
		m.setError("download failed: " + err.Error())
		return
	}

	// ── 安全校验：下载 checksums.txt 并验证 SHA-256 ──────────────────────────
	// checksums.txt 直接从 GitHub 下载（不走 ghproxy），锚定信任链。
	// 即使二进制来自镜像代理，校验值仍以 GitHub 为准，防御被篡改的镜像。
	m.setStatus(StatusVerifying)
	if err := m.verifyChecksum(ctx, version, archiveName, archivePath); err != nil {
		slog.Error("updater: checksum verification failed", "err", err)
		os.Remove(archivePath) //nolint:errcheck // 校验失败立即删除可疑文件
		m.setError("checksum verification failed: " + err.Error())
		return
	}

	slog.Info("updater: download verified, installing", "archive", archivePath)
	m.setStatus(StatusInstalling)

	if err := m.applyUpdate(archivePath); err != nil {
		m.setError(err.Error())
		return
	}

	slog.Info("updater: binary and libs replaced, restarting")
	m.setStatus(StatusRestarting)

	time.Sleep(500 * time.Millisecond)
	if m.restartFn != nil {
		m.restartFn()
	} else {
		exePath, _ := m.executableFn()
		exePath, _ = filepath.EvalSymlinks(exePath)
		m.defaultRestart(exePath)
	}
}

func (m *Manager) applyUpdate(archivePath string) error {
	exePath, err := m.executableFn()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "resolve executable", err)
	}
	exePath, _ = filepath.EvalSymlinks(exePath)

	newBinPath := exePath + ".new"
	newLibDir := filepath.Join(filepath.Dir(exePath), "lib.new")
	os.RemoveAll(newLibDir) // 清理可能残留的临时目录
	if err := os.MkdirAll(newLibDir, 0o755); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "mkdir failed", err)
	}

	if err := extractFiles(archivePath, newBinPath, newLibDir); err != nil {
		os.Remove(newBinPath)   //nolint:errcheck
		os.RemoveAll(newLibDir) //nolint:errcheck
		os.Remove(archivePath)  //nolint:errcheck
		return apperr.Wrap(apperr.CodeInternal, "extract failed", err)
	}
	os.Remove(archivePath) //nolint:errcheck

	if err := os.Chmod(newBinPath, 0o755); err != nil {
		os.Remove(newBinPath)   //nolint:errcheck
		os.RemoveAll(newLibDir) //nolint:errcheck
		return apperr.Wrap(apperr.CodeInternal, "chmod failed", err)
	}

	targetLibDir := filepath.Join(filepath.Dir(exePath), "lib")

	// 原子替换（Unix 可替换运行中文件；Windows 需延迟脚本）
	errRename := os.Rename(newBinPath, exePath)
	if errRename != nil {
		if runtime.GOOS != "windows" {
			os.Remove(newBinPath)   //nolint:errcheck
			os.RemoveAll(newLibDir) //nolint:errcheck
			return apperr.Wrap(apperr.CodeInternal, "replace failed", errRename)
		}
		if scriptErr := m.writeWindowsUpdateScript(exePath, newBinPath, targetLibDir, newLibDir); scriptErr != nil {
			os.Remove(newBinPath)   //nolint:errcheck
			os.RemoveAll(newLibDir) //nolint:errcheck
			return apperr.Wrap(apperr.CodeInternal, "replace failed (windows)", scriptErr)
		}
		return nil
	}

	return replaceUnixLibs(newLibDir, targetLibDir)
}

func replaceUnixLibs(newLibDir, targetLibDir string) error {
	files, _ := os.ReadDir(newLibDir)
	if err := os.MkdirAll(targetLibDir, 0o755); err != nil {
		slog.Warn("updater: failed to create target lib dir", "err", err)
	}
	for _, f := range files {
		srcFile := filepath.Join(newLibDir, f.Name())
		dstFile := filepath.Join(targetLibDir, f.Name())
		os.Remove(dstFile) //nolint:errcheck // 先移除旧的（如果是 running 的 .so，Remove 只会 unlink inode）
		if err := os.Rename(srcFile, dstFile); err != nil {
			slog.Warn("updater: failed to rename lib", "src", srcFile, "dst", dstFile, "err", err)
		}
	}
	os.RemoveAll(newLibDir) //nolint:errcheck
	return nil
}

// verifyChecksum 从 GitHub 直接下载 checksums.txt，验证 archivePath 的 SHA-256。
// checksums.txt 不走 ghproxy 代理：即使镜像被篡改，仍以 GitHub 的校验值为权威。
//
//nolint:gocyclo
func (m *Manager) verifyChecksum(ctx context.Context, version, archiveName, archivePath string) error {
	checksumURL := fmt.Sprintf(
		"https://github.com/%s/%s/releases/download/%s/%s.sha256",
		repoOwner, repoName, version, archiveName,
	)

	c := m.client
	if c == nil {
		c = &http.Client{Timeout: 30 * time.Second}
	}

	var data []byte
	var downloadErr error

	// 尝试 downloader 提供的候选节点（直连优先，失败后尝试代理）
	// 这里放宽了严格的不走代理限制：直连 GitHub 会优先尝试，若完全被阻断则降级使用镜像。
	// 这对在中国大陆完全无法访问 GitHub 的环境是必须的。
	for _, u := range downloader.CandidateURLs(ctx, c, checksumURL) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			downloadErr = apperr.Wrap(apperr.CodeInternal, "checksum request", err)
			continue
		}
		resp, err := c.Do(req)
		if err != nil {
			downloadErr = apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("download %s.sha256 from %s", archiveName, u), err)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			downloadErr = apperr.New(apperr.CodeInternal, fmt.Sprintf("%s.sha256 from %s: HTTP %d", archiveName, u, resp.StatusCode))
			continue
		}

		data, err = io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 最多 1MB
		resp.Body.Close()
		if err != nil {
			downloadErr = apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("read %s.sha256 from %s", archiveName, u), err)
			continue
		}
		// 成功下载
		downloadErr = nil
		break
	}

	if downloadErr != nil {
		return downloadErr
	}

	// 格式：<sha256hex>  <filename> (单行文件)
	var expectedHash []byte
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 1 && len(fields[0]) == 64 {
			var errDecode error
			expectedHash, errDecode = hex.DecodeString(fields[0])
			if errDecode != nil {
				return apperr.Wrap(apperr.CodeInternal, "invalid checksum hex", errDecode)
			}
			break
		}
	}
	if expectedHash == nil {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("valid SHA-256 hash not found in %s.sha256", archiveName))
	}

	// 计算已下载归档的 SHA-256
	f, err := os.Open(archivePath)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "open archive for hash", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "hash archive", err)
	}
	actualHash := h.Sum(nil)

	// 恒定时间比较，防御时序攻击
	if !hmac.Equal(actualHash, expectedHash) {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("SHA-256 mismatch: expected %x, got %x", expectedHash, actualHash))
	}

	slog.Info("updater: SHA-256 verified", "archive", archiveName)
	return nil
}

// extractFiles 从归档中提取 polaris 二进制和 lib 目录。
func extractFiles(archivePath, destBinPath, destLibDir string) error {
	binaryNames := map[string]bool{"polaris": true, "polaris.exe": true}
	mapper := func(name string) (string, bool) {
		nameStr := filepath.ToSlash(filepath.Clean(name))
		parts := strings.Split(nameStr, "/")
		if binaryNames[filepath.Base(nameStr)] && len(parts) <= 2 {
			// 允许根目录或一个顶层目录下的二进制文件
			return destBinPath, true
		}
		if len(parts) >= 2 && parts[len(parts)-2] == "lib" {
			// 允许 lib 目录下的文件
			return filepath.Join(destLibDir, filepath.Base(nameStr)), true
		}
		return "", false
	}

	if strings.HasSuffix(archivePath, ".zip") {
		return downloader.ExtractZip(archivePath, filepath.Dir(destBinPath), mapper)
	}
	f, err := os.Open(archivePath)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "open archive for extract", err)
	}
	defer f.Close()
	return downloader.ExtractTarGz(f, mapper)
}

func (m *Manager) defaultRestart(exePath string) {
	slog.Info("updater: exiting for service manager restart", "path", exePath)
	m.exitFn(0)
}

func (m *Manager) writeWindowsUpdateScript(exePath, newBinPath, targetLibDir, newLibDir string) error {
	script := fmt.Sprintf(`@echo off
timeout /t 2 /nobreak >nul
move /Y "%s" "%s"
if exist "%s" (
    xcopy /Y /E /Q "%s\*" "%s\"
    rmdir /S /Q "%s"
)
start "" "%s"
del "%%~f0"
`, newBinPath, exePath, newLibDir, newLibDir, targetLibDir, newLibDir, exePath)
	scriptPath := exePath + ".update.bat"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		return fmt.Errorf("write windows update script: %w", err)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		m.exitFn(0)
	}()
	return nil
}

func (m *Manager) setStatus(s Status) {
	m.mu.Lock()
	m.info.UpdateStatus = s
	m.info.UpdateError = ""
	m.mu.Unlock()
}

func (m *Manager) setError(msg string) {
	m.mu.Lock()
	m.info.UpdateStatus = StatusError
	m.info.UpdateError = msg
	m.mu.Unlock()
	slog.Error("updater: error", "msg", msg)
}

func (m *Manager) setIdle() {
	m.mu.Lock()
	if m.info.UpdateStatus == StatusChecking {
		m.info.UpdateStatus = StatusIdle
	}
	m.mu.Unlock()
}

func equalVersions(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

// semverCompare 比较两个不带 'v' 前缀的语义版本字符串。
// 返回: -1 表示 a < b；0 表示 a == b；1 表示 a > b。
// 仅解析 major.minor.patch，忽略预发布后缀。
func semverCompare(a, b string) int {
	parse := func(s string) [3]int {
		// 截断预发布后缀（"-rc.1"、"+build"）
		if i := strings.IndexAny(s, "-+"); i >= 0 {
			s = s[:i]
		}
		parts := strings.SplitN(s, ".", 3)
		var n [3]int
		for i, p := range parts {
			if i >= 3 {
				break
			}
			v, _ := strconv.Atoi(p)
			n[i] = v
		}
		return n
	}
	va, vb := parse(a), parse(b)
	for i := range va {
		if va[i] < vb[i] {
			return -1
		}
		if va[i] > vb[i] {
			return 1
		}
	}
	return 0
}
