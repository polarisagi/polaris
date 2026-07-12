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
	// ephemeral 脚本孤儿巡检：进程崩溃/panic 导致 StageEphemeralFile 返回的
	// cleanup() 未被调用时的兜底回收，独立于 7 天周期的 GC()（后者面向持久
	// taskID 工作区，粒度太粗，不适合"预期秒级生命周期"的临时脚本）。
	concurrent.SafeGo(context.Background(), "vfs.ephemeral.sweep", func(ctx context.Context) {
		ticker := time.NewTicker(ephemeralSweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				wm.SweepEphemeralOrphans(int64(ephemeralOrphanMaxAge.Seconds()))
			}
		}
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

// createdAtMarkerFile 每个任务工作区目录下记录真实创建时间的隐藏标记文件
// （D-B6-03 修复）。原实现在进程重启后通过 rebuildManifests 用目录 ModTime
// 兜底 CreatedAt，而 ModTime 会被后续任意一次文件写入刷新，导致同一目录的
// GC 存活期在"进程不重启"（以创建时间计 7 天）与"进程重启"（以 ModTime
// 归零重计 7 天）两种情况下不一致——重启后可能人为延长任务工作区寿命，
// 或反过来因 ModTime 早于真实创建时间（罕见但可能）过早回收。
const createdAtMarkerFile = ".wm_created_at"

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
			if info.IsDir() || info.Name() == createdAtMarkerFile {
				return nil
			}
			totalSize += info.Size()
			files = append(files, WorkspaceFile{
				Path: path,
				Size: info.Size(),
			})
			return nil
		})
		// 优先读取持久化的真实创建时间标记；缺失时（如升级前已存在的旧目录）
		// 回退 ModTime 作为近似值，与修复前行为兼容。
		createdAt := readCreatedAtMarker(dir)
		if createdAt == 0 {
			if info, _ := e.Info(); info != nil {
				createdAt = info.ModTime().Unix()
			}
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

// writeCreatedAtMarker 在任务目录下写入创建时间标记（仅 Create() 首次创建时调用）。
func writeCreatedAtMarker(dir string, unixSec int64) {
	_ = os.WriteFile(filepath.Join(dir, createdAtMarkerFile), []byte(fmt.Sprint(unixSec)), 0o600)
}

// readCreatedAtMarker 读取任务目录下的创建时间标记；不存在或解析失败返回 0。
func readCreatedAtMarker(dir string) int64 {
	data, err := os.ReadFile(filepath.Join(dir, createdAtMarkerFile))
	if err != nil {
		return 0
	}
	var v int64
	if _, err := fmt.Sscanf(string(data), "%d", &v); err != nil {
		return 0
	}
	return v
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
	createdAt := time.Now().Unix()
	writeCreatedAtMarker(dir, createdAt) // D-B6-03：持久化真实创建时间，供重启后 rebuildManifests 读取
	wm.manifests[key] = &WorkspaceManifest{
		TaskID:    taskID,
		CreatedAt: createdAt,
	}
	return dir, nil
}

// RegisterFile 将文件记录到工作区 manifest，供 GC 使用。
// 注意：不再重复累加 wm.totalSize 全局原子计数器——该计数器的配额份额已在
// CheckQuota（预占式）阶段原子占用，此处重复累加会导致同一次写入被计两次
// 配额（D-B6-01 修复的一部分）。manifest 级 m.TotalSize 仍照常累加，供
// GC/巡检等按任务维度统计使用。
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
}

// CheckQuota 配额预占式检查（D-B6-01 修复：原实现仅读取快照，Check 通过后到
// RegisterFile 实际登记之间存在 TOCTOU 窗口，并发写入可无限突破 maxSize 硬限制）。
// 通过即代表已原子占用 pendingWrite 份额；调用方后续必须且只能二选一：
//  1. 写入成功 → 正常调用 RegisterFile 登记文件（不再重复占用配额）；
//  2. 写入失败/放弃 → 必须调用 ReleaseQuota(pendingWrite) 归还预占份额，
//     否则配额会永久泄漏。
func (wm *WorkspaceManager) CheckQuota(pendingWrite int64) error {
	total := atomic.AddInt64(&wm.totalSize, pendingWrite)
	if total > wm.maxSize {
		atomic.AddInt64(&wm.totalSize, -pendingWrite) // 回滚预占
		return ErrWorkspaceQuotaExhausted
	}
	return nil
}

// ReleaseQuota 归还 CheckQuota 预占但最终未通过 RegisterFile 登记的配额份额
// （写入失败/中途放弃场景下调用方必须调用，防止预占配额永久泄漏）。
func (wm *WorkspaceManager) ReleaseQuota(n int64) {
	atomic.AddInt64(&wm.totalSize, -n)
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
