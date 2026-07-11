package vfs

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// WorkspaceManager — 重型中间物文件系统。
// 架构文档: docs/arch/02-Storage-Fabric-深度选型.md §3
//
// GR-6-002 修复（2026-07-11）：新增 mu sync.RWMutex 保护 manifests map 并发读写。
// gcWorker 作为后台 goroutine 运行，与主线程 Create/RegisterFile/GC 等方法存在
// 无锁并发访问同一 map 的致命竞争——Go runtime 检测到后直接 fatal panic 无法 recover。
//
// GR-6-003 修复（2026-07-11）：新增 totalSize int64（atomic）提供 O(1) 配额查询。
// 原实现 CheckQuota 每次 O(N) 全量遍历 manifests，随活跃任务数增长成为热路径瓶颈。

type WorkspaceManager struct {
	rootDir   string // ~/.polarisagi/polaris/workspaces
	maxSize   int64  // Tier 0 = 500MB
	manifests map[string]*WorkspaceManifest
	gcCh      chan string  // Background GC queue
	mu        sync.RWMutex // 保护 manifests map 并发读写（GR-6-002）
	totalSize int64        // 所有 manifest TotalSize 之和，原子更新（GR-6-003）
}

// NewWorkspaceManager 创建 WorkspaceManager，rootDir 不存在时自动创建。
func NewWorkspaceManager(rootDir string, maxSize int64) *WorkspaceManager {
	_ = os.MkdirAll(rootDir, 0o700)
	wm := &WorkspaceManager{
		rootDir:   rootDir,
		maxSize:   maxSize,
		manifests: make(map[string]*WorkspaceManifest),
		gcCh:      make(chan string, 1000),
	}
	wm.rebuildManifests()
	// gcWorker 负责异步清理墓碑目录；panic 不应导致 tombstone 永久堆积，用 SafeGo 保护
	concurrent.SafeGo(context.Background(), "vfs.tombstone.gc", func(_ context.Context) {
		wm.gcWorker()
	})
	return wm
}

func (wm *WorkspaceManager) gcWorker() {
	for path := range wm.gcCh {
		_ = os.RemoveAll(path)
		// Sleep briefly to reduce I/O pressure on disk during background cleanup
		time.Sleep(100 * time.Millisecond)
	}
}

// rebuildManifests 扫描 rootDir 重建 manifests，避免重启后 quota/GC 失效。
// 仅在构造时调用（单线程），无需加锁，但调用后初始化 totalSize 原子计数器。
func (wm *WorkspaceManager) rebuildManifests() {
	entries, err := os.ReadDir(wm.rootDir)
	if err != nil {
		return
	}
	var total int64
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
		total += totalSize
	}
	// 一次性初始化 totalSize 原子计数器（rebuildManifests 只在构造时调用）
	atomic.StoreInt64(&wm.totalSize, total)
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

	wm.mu.Lock()
	defer wm.mu.Unlock()

	if _, exists := wm.manifests[key]; exists {
		return filepath.Join(wm.rootDir, key), nil // 幂等
	}
	dir := filepath.Join(wm.rootDir, key)
	// MkdirAll 是磁盘 IO，在锁内执行。由于 Create 不是高频热路径（每个任务仅创建一次），
	// 持锁期间短暂 IO 可接受。相比在锁外做 IO 后再锁内写 map 的 TOCTOU 风险，
	// 当前方式更安全：避免两个并发 Create(同一 taskID) 同时通过幂等检查然后双重创建。
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

	wm.mu.Lock()
	m, ok := wm.manifests[key]
	if !ok {
		wm.mu.Unlock()
		return
	}
	m.Files = append(m.Files, f)
	m.TotalSize += f.Size
	wm.mu.Unlock()

	// totalSize 原子更新（在锁外进行，atomic 操作无需 mu 保护）
	atomic.AddInt64(&wm.totalSize, f.Size)
}

// CheckQuota 写入前检查配额（O(1) 查询，GR-6-003 修复）。
// workspace_write 前 du 累积占用量 + 待写入 > maxSize → ErrQuotaExhausted
func (wm *WorkspaceManager) CheckQuota(pendingWrite int64) error {
	total := atomic.LoadInt64(&wm.totalSize)
	if total+pendingWrite > wm.maxSize {
		return ErrWorkspaceQuotaExhausted
	}
	return nil
}

// WriteFile 将 data 写入相对路径 relPath（基于 RootDir），自动创建父目录。
func (wm *WorkspaceManager) WriteFile(relPath string, data []byte) error {
	fullPath := filepath.Join(wm.rootDir, relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0700); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "WorkspaceManager.WriteFile: failed to mkdir", err)
	}
	if err := os.WriteFile(fullPath, data, 0600); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "WorkspaceManager.WriteFile: failed to write file", err)
	}
	return nil
}

// ReadFile 从相对路径 relPath 读取文件，最多读取 limit 字节。如果 limit <= 0，读取全部。
func (wm *WorkspaceManager) ReadFile(relPath string, limit int64) ([]byte, error) {
	fullPath := filepath.Join(wm.rootDir, relPath)
	f, err := os.Open(fullPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeNotFound, "WorkspaceManager.ReadFile: failed to open file", err)
	}
	defer f.Close()

	if limit <= 0 {
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "WorkspaceManager.ReadFile: failed to read all", err)
		}
		return data, nil
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "WorkspaceManager.ReadFile: failed to read limit", err)
	}
	return data, nil
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

	wm.mu.Lock()
	defer wm.mu.Unlock()

	for key, m := range wm.manifests {
		if _, isActive := active[key]; isActive {
			continue // 活跃任务工作区不回收
		}
		if now-m.CreatedAt <= maxAgeSecs {
			continue
		}
		dir := filepath.Join(wm.rootDir, key)
		tombPath := dir + ".tombstone." + fmt.Sprint(now)
		// 磁盘 IO（Rename/RemoveAll）在锁内执行。GC 是低频后台操作（调用间隔通常为小时级），
		// 且 Rename 通常是原子 syscall（无实际数据移动），持锁期间的 IO 代价可接受。
		// 若未来 GC 成为性能瓶颈，可改为：锁内只收集待删列表，锁外执行磁盘 IO。
		if err := os.Rename(dir, tombPath); err == nil {
			select {
			case wm.gcCh <- tombPath:
			default:
				_ = os.RemoveAll(tombPath) // queue full, delete synchronously
			}
		} else {
			_ = os.RemoveAll(dir) // rename failed, fallback to direct delete
		}
		// 从原子计数器中减去回收的空间
		atomic.AddInt64(&wm.totalSize, -m.TotalSize)
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
