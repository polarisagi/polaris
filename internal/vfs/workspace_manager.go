package vfs

import (
	"os"
	"path/filepath"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// WorkspaceManager — 重型中间物文件系统。
// 架构文档: docs/arch/02-Storage-Fabric-深度选型.md §3

type WorkspaceManager struct {
	rootDir   string // ~/.polarisagi/polaris/workspaces
	maxSize   int64  // Tier 0 = 500MB
	manifests map[string]*WorkspaceManifest
}

// NewWorkspaceManager 创建 WorkspaceManager，rootDir 不存在时自动创建。
func NewWorkspaceManager(rootDir string, maxSize int64) *WorkspaceManager {
	_ = os.MkdirAll(rootDir, 0o700)
	wm := &WorkspaceManager{
		rootDir:   rootDir,
		maxSize:   maxSize,
		manifests: make(map[string]*WorkspaceManifest),
	}
	wm.rebuildManifests()
	return wm
}

// rebuildManifests 扫描 rootDir 重建 manifests，避免重启后 quota/GC 失效。
func (wm *WorkspaceManager) rebuildManifests() {
	entries, err := os.ReadDir(wm.rootDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		taskID := e.Name()
		dir := filepath.Join(wm.rootDir, taskID)
		var totalSize int64
		var files []WorkspaceFile
		_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return nil
			}
			totalSize += info.Size()
			files = append(files, WorkspaceFile{
				Path: path,
				Size: info.Size(),
			})
			return nil
		})
		var createdAt int64
		if info, _ := e.Info(); info != nil {
			createdAt = info.ModTime().Unix()
		}
		wm.manifests[taskID] = &WorkspaceManifest{
			TaskID:    taskID,
			CreatedAt: createdAt,
			Files:     files,
			TotalSize: totalSize,
		}
	}
}

// GetRootDir 返回工作区根目录。
func (wm *WorkspaceManager) GetRootDir() string {
	return wm.rootDir
}

type WorkspaceManifest struct {
	TaskID    string
	CreatedAt int64
	Files     []WorkspaceFile
	TotalSize int64
}

type WorkspaceFile struct {
	Path        string
	Size        int64
	Summary     string // ~50 字
	ContentType string
}

// Create 为任务创建隔离工作区目录，并注册 manifest。
// 目录路径: {rootDir}/{taskID}/，权限 0700（仅当前进程可读写）。
func (wm *WorkspaceManager) Create(taskID string) (string, error) {
	key := filepath.Base(filepath.Clean(taskID))
	if key == "." || key == "/" || key == "\\" {
		return "", apperr.New(apperr.CodeInvalidInput, "invalid taskID")
	}

	if _, exists := wm.manifests[key]; exists {
		return filepath.Join(wm.rootDir, key), nil // 幂等
	}
	dir := filepath.Join(wm.rootDir, key)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "WorkspaceManager.Create", err)
	}
	wm.manifests[key] = &WorkspaceManifest{
		TaskID:    taskID,
		CreatedAt: time.Now().Unix(),
	}
	return dir, nil
}

// RegisterFile 将文件记录到工作区 manifest，供 CheckQuota 和 GC 使用。
func (wm *WorkspaceManager) RegisterFile(taskID string, f WorkspaceFile) {
	key := filepath.Base(filepath.Clean(taskID))
	m, ok := wm.manifests[key]
	if !ok {
		return
	}
	m.Files = append(m.Files, f)
	m.TotalSize += f.Size
}

// CheckQuota 写入前检查配额。
// workspace_write 前 du 累积占用量 + 待写入 > maxSize → ErrQuotaExhausted
func (wm *WorkspaceManager) CheckQuota(pendingWrite int64) error {
	var total int64
	for _, m := range wm.manifests {
		total += m.TotalSize
	}
	if total+pendingWrite > wm.maxSize {
		return ErrWorkspaceQuotaExhausted
	}
	return nil
}

// GC 回收 > 7 天的 workspace 目录。
// activeTaskIDs 是调用方传入的当前仍活跃（running/suspended）任务 ID 集合；
// 活跃任务的 workspace 无论年龄多大都不删除，防止删除正在运行的持久战任务数据。
// now 为 Unix 秒，由调用方传入，便于测试覆盖。
func (wm *WorkspaceManager) GC(now int64, activeTaskIDs []string) {
	const maxAgeSecs = 7 * 86400

	// 构建活跃任务 ID 集合，O(1) 查找
	active := make(map[string]struct{}, len(activeTaskIDs))
	for _, id := range activeTaskIDs {
		active[id] = struct{}{}
	}

	for key, m := range wm.manifests {
		if _, isActive := active[key]; isActive {
			continue // 活跃任务工作区不回收
		}
		if now-m.CreatedAt <= maxAgeSecs {
			continue
		}
		dir := filepath.Join(wm.rootDir, key)
		_ = os.RemoveAll(dir)
		delete(wm.manifests, key)
	}
}

// DirPath 返回任务工作区的物理路径（不创建）。
func (wm *WorkspaceManager) DirPath(taskID string) string {
	key := filepath.Base(filepath.Clean(taskID))
	return filepath.Join(wm.rootDir, key)
}

var ErrWorkspaceQuotaExhausted = &WorkspaceError{"workspace quota exhausted"}

type WorkspaceError struct{ msg string }

func (e *WorkspaceError) Error() string { return e.msg }
