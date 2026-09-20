package vfs

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// WorkspaceManager — 重型中间物文件系统。
// 架构文档: docs/arch/M02-Storage-Fabric.md §3
//
// GR-6-002 修复（2026-07-11）：新增 mu sync.RWMutex 保护 manifests map 并发读写。
// gcWorker 作为后台 goroutine 运行，与主线程 Create/RegisterFile/GC 等方法存在
// 无锁并发访问同一 map 的致命竞争——Go runtime 检测到后直接 fatal panic 无法 recover。
//
// GR-6-003 修复（2026-07-11）：新增 totalSize int64（atomic）提供 O(1) 配额查询。
// 原实现 CheckQuota 每次 O(N) 全量遍历 manifests，随活跃任务数增长成为热路径瓶颈。

type WorkspaceManager struct {
	rootDir   string
	cfg       config.M7ToolThresholds // ~/.polarisagi/polaris/workspaces
	maxSize   int64                   // Tier 0 = 500MB
	manifests map[string]*WorkspaceManifest
	gcCh      chan string  // Background GC queue
	mu        sync.RWMutex // 保护 manifests map 并发读写（GR-6-002）
	totalSize int64        // 所有 manifest TotalSize 之和，原子更新（GR-6-003）
	sf        singleflight.Group
}

// NewWorkspaceManager 创建 WorkspaceManager，rootDir 不存在时自动创建。
func NewWorkspaceManager(rootDir string, maxSize int64, cfg config.M7ToolThresholds) *WorkspaceManager {
	return NewWorkspaceManagerWithContext(context.Background(), rootDir, maxSize, cfg)
}

func NewWorkspaceManagerWithContext(parentCtx context.Context, rootDir string, maxSize int64, cfg config.M7ToolThresholds) *WorkspaceManager {
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	// 构造函数无 error 返回（调用方为组合根，历史签名），但根目录创建失败
	// 意味着后续所有任务沙箱目录都建不出来——必须留痕，否则表现为"每个工具
	// 调用各自报一个看不出根因的路径错误"。
	if err := os.MkdirAll(rootDir, 0o700); err != nil {
		slog.Error("vfs: failed to create workspace root, all task sandboxes will fail",
			"root_dir", rootDir, "err", err)
	}
	wm := &WorkspaceManager{
		rootDir:   rootDir,
		cfg:       cfg,
		maxSize:   maxSize,
		manifests: make(map[string]*WorkspaceManifest),
		gcCh:      make(chan string, 1000),
	}
	wm.rebuildManifests()
	// gcWorker 负责异步清理墓碑目录；panic 不应导致 tombstone 永久堆积，用 SafeGo 保护
	concurrent.SafeGo(parentCtx, "vfs.tombstone.gc", func(ctx context.Context) {
		wm.gcWorker(ctx)
	})
	// ephemeral 脚本孤儿巡检：进程崩溃/panic 导致 StageEphemeralFile 返回的
	// cleanup() 未被调用时的兜底回收，独立于 7 天周期的 GC()（后者面向持久
	// taskID 工作区，粒度太粗，不适合"预期秒级生命周期"的临时脚本）。
	concurrent.SafeGo(parentCtx, "vfs.ephemeral.sweep", func(ctx context.Context) {
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

// gcWorker 消费墓碑队列直到 ctx 取消（GR-6.1-003）：原实现 for-range 一个永不
// 关闭的 channel 且丢弃 ctx，进程内每构造一个 WorkspaceManager（热重启、测试）
// 就泄漏一个常驻 goroutine。不关闭 gcCh 而是监听 ctx：GC() 仍可能在关停窗口
// 并发投递，关闭 channel 会让投递方 panic；未消费的墓碑目录由下次启动的
// rebuildManifests 识别并回收。
func (wm *WorkspaceManager) gcWorker(ctx context.Context) {
	for {
		var path string
		select {
		case <-ctx.Done():
			return
		case path = <-wm.gcCh:
		}
		// 后台回收失败只告警不重试：目录会留到下一次同 taskID 复用或人工清理，
		// 不影响正确性；但持续失败意味着磁盘在泄漏，必须可观测。
		if err := os.RemoveAll(path); err != nil {
			slog.Warn("vfs: workspace gc failed, directory leaked", "path", path, "err", err)
		}
		// 两次回收之间短暂停顿以削峰磁盘 IO；用 select 使关停不必等满间隔
		select {
		case <-ctx.Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// createdAtMarkerFile 每个任务工作区目录下记录真实创建时间的隐藏标记文件
// （D-B6-03 修复）。原实现在进程重启后通过 rebuildManifests 用目录 ModTime
// 兜底 CreatedAt，而 ModTime 会被后续任意一次文件写入刷新，导致同一目录的
// GC 存活期在"进程不重启"（以创建时间计 7 天）与"进程重启"（以 ModTime
// 归零重计 7 天）两种情况下不一致——重启后可能人为延长任务工作区寿命，
// 或反过来因 ModTime 早于真实创建时间（罕见但可能）过早回收。
const createdAtMarkerFile = ".wm_created_at"

// tombstoneInfix GC 把过期工作区原子改名为 "<dir>.tombstone.<unix>" 后异步删除。
const tombstoneInfix = ".tombstone."

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
		// GR-6.1-009：根目录下并非每个子目录都是任务工作区——
		//   - 墓碑目录（上次进程在 gcWorker 消费前退出遗留）：直接重新入队回收；
		//   - _ephemeral_scripts：由 SweepEphemeralOrphans 按秒级寿命独立管理；
		//   - 其余无 createdAt 标记的目录（如 episodic 溢出载荷 logs/events/）是
		//     WriteFile 直写的持久数据，不属于任何 taskID，若按任务建清单会被
		//     7 天 GC 当作过期工作区整目录删除，造成情景记忆载荷永久丢失。
		// 只有 Create() 写过标记文件的目录才是可回收的任务工作区。
		if strings.Contains(taskID, tombstoneInfix) {
			select {
			case wm.gcCh <- dir:
			default:
				slog.Warn("vfs: gc queue full at startup, stale tombstone left for next restart", "dir", dir)
			}
			continue
		}
		if taskID == ephemeralScriptsSubdir {
			continue
		}
		if readCreatedAtMarker(dir) == 0 {
			continue
		}
		var totalSize int64
		var files []WorkspaceFile
		// Walk 错误只影响单个任务清单的完整性（下方按 files/totalSize 重建），
		// 不阻断其余任务的重建循环；但静默丢弃会让"清单少了文件"无从追查。
		walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
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
		if walkErr != nil {
			slog.Warn("vfs: workspace manifest rebuild walk failed, manifest may be incomplete",
				"task_id", taskID, "dir", dir, "err", walkErr)
		}
		// 上方已过滤无标记目录，这里的 createdAt 必为持久化的真实创建时间
		createdAt := readCreatedAtMarker(dir)
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
	if err := os.WriteFile(filepath.Join(dir, createdAtMarkerFile), []byte(fmt.Sprint(unixSec)), 0o600); err != nil {
		slog.Warn("vfs: failed to write created_at marker", "dir", dir, "err", err)
	}
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

	wm.mu.RLock()
	if _, exists := wm.manifests[key]; exists {
		wm.mu.RUnlock()
		return filepath.Join(wm.rootDir, key), nil // 幂等
	}
	wm.mu.RUnlock()

	dir := filepath.Join(wm.rootDir, key)

	_, err, _ := wm.sf.Do(key, func() (any, error) {
		// Double check inside singleflight
		wm.mu.RLock()
		if _, exists := wm.manifests[key]; exists {
			wm.mu.RUnlock()
			return nil, nil
		}
		wm.mu.RUnlock()

		// MkdirAll 是磁盘 IO，在锁外执行，避免阻塞其他任务的 Create/GC
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "WorkspaceManager.Create: mkdir", err)
		}
		createdAt := time.Now().Unix()
		writeCreatedAtMarker(dir, createdAt) // D-B6-03：持久化真实创建时间

		wm.mu.Lock()
		defer wm.mu.Unlock()
		if _, exists := wm.manifests[key]; !exists {
			wm.manifests[key] = &WorkspaceManifest{
				TaskID:    taskID,
				CreatedAt: createdAt,
			}
		}
		return nil, nil
	})

	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "WorkspaceManager.Create", err)
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

func resolveWithinRoot(rootDir, relPath string) (string, error) {
	if relPath == "" || filepath.IsAbs(relPath) {
		return "", apperr.New(apperr.CodeInvalidInput, "path must be a non-empty relative path")
	}
	fullPath := filepath.Join(rootDir, relPath)
	rel, err := filepath.Rel(rootDir, fullPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", apperr.New(apperr.CodeInvalidInput, "path traversal detected")
	}
	return fullPath, nil
}

// WriteFile 将 data 写入相对路径 relPath（基于 RootDir），自动创建父目录。
// 用 SafeOpenFile（O_NOFOLLOW，见 vfs_unix.go）替代 os.WriteFile：relPath 段来自
// Agent/工具产出，若工作区内已存在指向沙箱外的符号链接，裸 os.WriteFile 会跟随
// 写入任意宿主路径（2026-07-13 deadcode 复核发现 SafeOpen/SafeOpenFile 已实现
// 但从未被本文件的真实读写路径调用，是防护形同虚设的静默缺口）。
func (wm *WorkspaceManager) WriteFile(relPath string, data []byte) error {
	fullPath, err := resolveWithinRoot(wm.rootDir, relPath)
	if err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "WorkspaceManager.WriteFile", err)
	}
	if err := os.MkdirAll(filepath.Dir(fullPath), 0700); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "WorkspaceManager.WriteFile: failed to mkdir", err)
	}
	f, err := SafeOpenFile(fullPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "WorkspaceManager.WriteFile: failed to open file", err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "WorkspaceManager.WriteFile: failed to write file", err)
	}
	return nil
}

// ReadFile 从相对路径 relPath 读取文件，最多读取 limit 字节。如果 limit <= 0，读取全部。
// 用 SafeOpen（O_NOFOLLOW）替代 os.Open，理由同上 WriteFile 注释。
func (wm *WorkspaceManager) ReadFile(relPath string, limit int64) ([]byte, error) {
	fullPath, err := resolveWithinRoot(wm.rootDir, relPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "WorkspaceManager.ReadFile", err)
	}
	f, err := SafeOpen(fullPath)
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

// DirPath 返回任务工作区的物理路径（不创建）。
func (wm *WorkspaceManager) DirPath(taskID string) string {
	key := filepath.Base(filepath.Clean(taskID))
	return filepath.Join(wm.rootDir, key)
}
