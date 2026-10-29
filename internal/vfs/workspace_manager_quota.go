package vfs

import "sync/atomic"

// R7 拆分（阶段03 R-07）：配额预占相关方法（CheckQuota/ReleaseQuota/WithQuota）
// 与其错误类型从 workspace_manager.go 抽出至本文件，使主文件回落到 400 行
// 上限内；行为与拆分前逐行等价。

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

// WithQuota 以闭包方式管理配额预占（阶段03 R-07，GR-6-001 降级项防退化）：
// CheckQuota 通过后执行 fn；fn 返回非 nil error 或 panic 时自动
// ReleaseQuota 归还预占份额（panic 归还后重新 panic，不吞异常）；fn 返回 nil
// 时视为调用方已在 fn 内部通过 RegisterFile 完成登记，不归还。
//
// 新增写入路径一律使用本方法，禁止裸调 CheckQuota/ReleaseQuota 配对——该配对
// 写法依赖人工纪律"记得在每条失败路径都调用 ReleaseQuota"，是配额预占泄漏的
// 高发模式（即便当前两处既有调用方经审计均已正确配对，无实际泄漏，仍需闭包
// 收敛写法防止未来新增调用方重蹈覆辙）。internal/lint/ 的
// Test_inv_VFS_QuotaMustUseWithQuota 对此做静态扫描门控。
func (wm *WorkspaceManager) WithQuota(pendingWrite int64, fn func() error) (err error) {
	if qerr := wm.CheckQuota(pendingWrite); qerr != nil {
		return qerr
	}
	defer func() {
		if r := recover(); r != nil {
			wm.ReleaseQuota(pendingWrite)
			panic(r)
		}
		if err != nil {
			wm.ReleaseQuota(pendingWrite)
		}
	}()
	return fn()
}

var ErrWorkspaceQuotaExhausted = &WorkspaceError{"workspace quota exhausted"}

type WorkspaceError struct{ msg string }

func (e *WorkspaceError) Error() string { return e.msg }
