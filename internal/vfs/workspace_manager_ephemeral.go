package vfs

import (
	"os"
	"path/filepath"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// R7 拆分（2026-07-12）：批次4 XR-11 finding 复核修复新增的 StageEphemeralFile/
// SweepEphemeralOrphans 临时脚本落盘能力从 workspace_manager.go 抽出至本文件，
// 使主文件回落到 400 行上限内；行为与拆分前逐行等价。

// ephemeralScriptsSubdir 是 StageEphemeralFile 落盘的固定子目录名，独立于
// manifests 追踪的持久 taskID 工作区；SweepEphemeralOrphans 只扫描该子树。
const ephemeralScriptsSubdir = "_ephemeral_scripts"

// ephemeralOrphanMaxAge 孤儿临时脚本的判定阈值：正常路径下 cleanup() 会在单次
// 请求内（CodeAct CPUQuotaMs 上限 30s）同步删除文件；此阈值需显著大于最长单次
// 执行时长，避免把仍在执行中的合法文件误判为孤儿。
const ephemeralOrphanMaxAge = 1 * time.Hour

// ephemeralSweepInterval 孤儿清理巡检周期。
const ephemeralSweepInterval = 10 * time.Minute

// StageEphemeralFile 写入一次性临时文件：配额预占（CheckQuota）+ 落盘（0700，
// 兼容 CodeAct bash 脚本执行位）+ 返回绝对路径与清理闭包。
//
// 设计动机（批次4 XR-11 finding 复核修复）：CodeAct 等需要向 OS 级沙箱
// （Rust bwrap/Seatbelt CmdRunner）传递真实文件路径的调用方，此前直接
// os.CreateTemp("", ...) 写入系统级 /tmp——脱离 VFS 隔离边界与配额核算
// （HE-6），且路径落在系统共享临时目录而非按会话隔离的工作区内。
// 沙箱执行器本身要求宿主文件系统上的真实路径（bind-mount/allow-read 需要），
// 无法回避落盘这一步；系统最优方案是把落盘目标迁移到 WorkspaceManager 管辖
// 的 rootDir 之下，使其纳入既有配额/GC 体系，而不是假装可以完全不碰宿主
// 文件系统。
//
// namespace 建议使用 SessionID/AgentID 等调用方维度标识（经净化，防路径穿越），
// filename 为具体文件名（如 "<uuid>.py"）；实际落盘路径为
// "<rootDir>/_ephemeral_scripts/<namespace>/<filename>"，父目录不存在时自动创建。
// 与持久化 artifact（WriteFile+RegisterFile）不同，本方法面向"写入→执行→
// 立即删除"的单次执行场景：cleanup() 会同时删除文件并归还预占配额，调用方
// 必须在使用完毕后调用（通常 defer），否则配额会像文件被遗留一样被长期占用
// ——SweepEphemeralOrphans 提供进程崩溃场景下的兜底回收。
func (wm *WorkspaceManager) StageEphemeralFile(namespace, filename string, data []byte) (absPath string, cleanup func(), err error) {
	safeNS := filepath.Base(filepath.Clean(namespace))
	if safeNS == "" || safeNS == "." || safeNS == string(filepath.Separator) {
		safeNS = "default"
	}

	size := int64(len(data))
	if qerr := wm.CheckQuota(size); qerr != nil {
		return "", nil, qerr
	}

	full := filepath.Join(wm.rootDir, ephemeralScriptsSubdir, safeNS, filename)
	if mkErr := os.MkdirAll(filepath.Dir(full), 0700); mkErr != nil {
		wm.ReleaseQuota(size)
		return "", nil, apperr.Wrap(apperr.CodeInternal, "WorkspaceManager.StageEphemeralFile: mkdir", mkErr)
	}
	// 0700：CodeAct bash 脚本场景需要执行位；解释器均以显式命令形式调用
	// （如 "bash <path>"），执行位并非严格必需，但保留以维持与旧
	// os.CreateTemp+os.Chmod(0700) 路径等价的权限语义，避免行为细节回归。
	if wErr := os.WriteFile(full, data, 0700); wErr != nil {
		wm.ReleaseQuota(size)
		return "", nil, apperr.Wrap(apperr.CodeInternal, "WorkspaceManager.StageEphemeralFile: write", wErr)
	}

	cleanup = func() {
		_ = os.Remove(full)
		wm.ReleaseQuota(size)
	}
	return full, cleanup, nil
}

// SweepEphemeralOrphans 删除 _ephemeral_scripts/ 子树下超过 maxAgeSecs 未被正常
// cleanup() 回收的孤儿文件（进程崩溃/panic 导致 defer cleanup() 未执行的边界
// 场景兜底）。独立于 GC()（后者仅回收 manifests 登记的持久 taskID 工作区，
// 且以 7 天为周期）——ephemeral 脚本预期生命周期是单次请求，孤儿判定阈值
// 需要短得多，由 NewWorkspaceManager 内的后台 ticker 定期调用。
func (wm *WorkspaceManager) SweepEphemeralOrphans(maxAgeSecs int64) {
	root := filepath.Join(wm.rootDir, ephemeralScriptsSubdir)
	maxAge := time.Duration(maxAgeSecs) * time.Second
	now := time.Now()
	_ = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info == nil || info.IsDir() {
			return nil //nolint:nilerr // 单个条目读取失败不应中断整棵子树的巡检
		}
		if now.Sub(info.ModTime()) <= maxAge {
			return nil
		}
		// 仅在真正删除成功时才归还配额，避免与正常路径 cleanup() 的极小概率
		// 竞争窗口下重复释放（若 cleanup() 先行删除，本次 os.Remove 会失败）。
		if rmErr := os.Remove(path); rmErr == nil {
			wm.ReleaseQuota(info.Size())
		}
		return nil
	})
}
