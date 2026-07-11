// Package updater 实现 polaris 的自更新（OTA）功能。
// 流程：查询 GitHub Releases API → 比较版本 → 下载（支持断点续传）→ SHA-256 校验 → 原子替换 → 重启进程。
package updater

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/downloader"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// 安装机制（applyUpdate/SHA-256 校验/归档提取/Windows 延迟替换脚本/版本比较）
// 见 updater_install.go（R7 拆分）。

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
		exitFn: func(code int) {
			slog.Warn("updater: restart requested, exiting", "code", code)
			os.Exit(code)
		},
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

	var hasUpdate bool
	if latest != "" {
		if current == "dev" {
			hasUpdate = true
		} else {
			cur := strings.TrimPrefix(current, "v")
			tgt := strings.TrimPrefix(latest, "v")
			hasUpdate = semverCompare(tgt, cur) > 0
		}
	}

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
	concurrent.SafeGo(ctx, "sysmgr.updater.background_check", func(ctx context.Context) {
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
	})
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

	ctxUpdate, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	concurrent.SafeGo(ctxUpdate, "sysmgr.updater.trigger_update", func(ctx context.Context) {
		defer cancel()
		m.doUpdate(ctx, version)
	})
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
