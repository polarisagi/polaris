package vfs

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/polarisagi/polaris/pkg/concurrent"
)

// workspaceGCInterval 周期 GC 的扫描间隔。工作区存活期以天计（默认 7 天），
// 小时级扫描足以把回收延迟控制在存活期的 1% 以内，且 GC 持锁遍历 manifests
// 的代价在此频率下可忽略。
const workspaceGCInterval = time.Hour

// ActiveTaskIDsFunc 返回当前仍活跃（非终态）的任务 / 会话 ID 集合。
// 由组合根注入（通常查询 tasks 表），vfs 不直接依赖存储层。
type ActiveTaskIDsFunc func(ctx context.Context) ([]string, error)

// StartPeriodicGC 启动后台周期 GC（GR-6.1-004）。此前 GC() 生产零调用，
// 任务工作区（tool_refs 卸载文件等）只增不减，最终撑满 Tier0 配额后所有
// 写入都被 CheckQuota 拒绝。
//
// activeIDs 查询失败时本轮跳过（fail-closed）：宁可推迟回收，也不能在无法
// 确认任务状态时删除可能仍在运行的持久战任务工作区。
func (wm *WorkspaceManager) StartPeriodicGC(ctx context.Context, activeIDs ActiveTaskIDsFunc) {
	if activeIDs == nil {
		slog.Warn("vfs: periodic workspace GC disabled: no active-task provider")
		return
	}
	concurrent.SafeGo(ctx, "vfs.workspace.gc", func(ctx context.Context) {
		ticker := time.NewTicker(workspaceGCInterval)
		defer ticker.Stop()
		for {
			wm.runGCOnce(ctx, activeIDs)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})
}

func (wm *WorkspaceManager) runGCOnce(ctx context.Context, activeIDs ActiveTaskIDsFunc) {
	ids, err := activeIDs(ctx)
	if err != nil {
		slog.Warn("vfs: workspace GC skipped, active task query failed", "err", err)
		return
	}
	wm.GC(time.Now().Unix(), ids)
}

// GC 回收 > 7 天的 workspace 目录。
// activeTaskIDs 是调用方传入的当前仍活跃（running/suspended）任务 ID 集合；
// 活跃任务的 workspace 无论年龄多大都不删除，防止删除正在运行的持久战任务数据。
// now 为 Unix 秒，由调用方传入，便于测试覆盖。
func (wm *WorkspaceManager) GC(now int64, activeTaskIDs []string) {
	maxAgeSecs := int64(wm.cfg.WorkspaceMaxAgeSeconds)

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
		tombPath := dir + tombstoneInfix + fmt.Sprint(now)
		// 磁盘 IO（Rename/RemoveAll）在锁内执行。GC 是低频后台操作（调用间隔通常为小时级），
		// 且 Rename 通常是原子 syscall（无实际数据移动），持锁期间的 IO 代价可接受。
		// 若未来 GC 成为性能瓶颈，可改为：锁内只收集待删列表，锁外执行磁盘 IO。
		if err := os.Rename(dir, tombPath); err == nil {
			select {
			case wm.gcCh <- tombPath:
			default:
				if errRm := os.RemoveAll(tombPath); errRm != nil {
					slog.Warn("vfs: gc remove tombPath failed synchronously", "tombPath", tombPath, "err", errRm)
				}
			}
		} else {
			if errRm := os.RemoveAll(dir); errRm != nil {
				slog.Warn("vfs: gc remove dir failed on rename fallback", "dir", dir, "err", errRm)
			}
		}
		// 从原子计数器中减去回收的空间
		atomic.AddInt64(&wm.totalSize, -m.TotalSize)
		delete(wm.manifests, key)
	}
}
