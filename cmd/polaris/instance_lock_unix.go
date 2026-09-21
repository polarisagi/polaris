//go:build unix

package main

import (
	"log/slog"
	"os"
	"syscall"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// lockInstance 以非阻塞方式独占 path 对应的锁文件。
//
// 用 flock 而非"写 PID 文件后探活"：PID 会被操作系统复用，陈旧 PID 文件撞上无关
// 进程时探活给出的是错误结论；而 flock 由内核在进程退出（含被 kill -9、崩溃）时
// 自动释放，不存在陈旧状态。锁文件内容无意义，判据是锁本身。
func lockInstance(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // 路径来自 config.DataLayout
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "单实例锁：打开锁文件失败 "+path, err)
	}
	// 短暂重试：`polaris service status --json` 判活时会以微秒级时长试探这把锁
	// （probeInstanceLock），恰好撞上的话，一次失败就放弃会让守护进程因为一次
	// 状态查询而拒绝启动。真正的第二实例在整个重试窗口内都拿不到锁，结论不变。
	for attempt := 0; ; attempt++ {
		if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return f, nil
		}
		if attempt >= lockRetries {
			break
		}
		time.Sleep(lockRetryInterval)
	}
	if cerr := f.Close(); cerr != nil {
		slog.Warn("单实例锁：关闭锁文件失败", "err", cerr)
	}
	return nil, apperr.Wrap(apperr.CodeAlreadyExists, "单实例锁：已有实例持有锁 "+path, err)
}

// probeInstanceLock 判定是否有进程持有锁，自身不长期占用。
//
// 这是"守护进程是否在运行"的权威判据：run/ 下的端口与令牌文件在 kill -9 后会残留，
// 按文件存在判活会把一个已死的进程报成"运行中"，客户端随后连向一个没人监听的端口。
// 锁由内核在进程退出时释放，不存在陈旧状态。
func probeInstanceLock(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600) //nolint:gosec // 路径来自 config.DataLayout
	if err != nil {
		return false // 锁文件都不存在：从未运行过
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			slog.Debug("单实例锁探测：关闭失败", "err", cerr)
		}
	}()
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return true
	}
	// 拿到了说明没人持有；立刻释放（Close 亦会释放，显式解锁把窗口压到最短）。
	if uerr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); uerr != nil {
		slog.Debug("单实例锁探测：解锁失败，将随关闭释放", "err", uerr)
	}
	return false
}

// unlockInstance 释放锁并关闭文件。
//
// 不显式 LOCK_UN：关闭 fd 即释放该 open file description 上的 flock，少一次可能
// 失败的系统调用。刻意不删除锁文件：删除与其他进程的 open+flock 之间存在竞态
// （对方可能锁住一个已被 unlink 的 inode，于是两个进程各自"持锁"成功）。
// 留一个空文件在 run/ 下没有任何代价。
func unlockInstance(f *os.File) {
	if f == nil {
		return
	}
	// Close 失败只可能是 fd 已失效（锁同样已释放），但必须留痕：静默丢弃会让
	// "锁没放掉"与"根本没走到这一步"在日志里长得一模一样（HE-1）。
	if err := f.Close(); err != nil {
		slog.Warn("单实例锁：关闭锁文件失败，锁将随进程退出释放", "err", err)
	}
}
