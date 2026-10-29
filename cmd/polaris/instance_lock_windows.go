//go:build windows

package main

import (
	"log/slog"
	"os"
	"time"

	"golang.org/x/sys/windows"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// lockInstance 见 instance_lock_unix.go 的设计说明；Windows 用 LockFileEx 取得
// 等价语义（独占 + 立即失败），句柄关闭或进程退出时由系统释放。
func lockInstance(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // 路径来自 config.DataLayout
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "单实例锁：打开锁文件失败 "+path, err)
	}
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	// 短暂重试，理由同 unix 侧。
	for attempt := 0; ; attempt++ {
		ol := new(windows.Overlapped)
		if err = windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, ol); err == nil {
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

// probeInstanceLock 判定是否有进程持有锁，自身不长期占用（理由见 unix 侧）。
func probeInstanceLock(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0o600) //nolint:gosec // 路径来自 config.DataLayout
	if err != nil {
		return false
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			slog.Debug("单实例锁探测：关闭失败", "err", cerr)
		}
	}()
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
	ol := new(windows.Overlapped)
	if err = windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, ol); err != nil {
		return true
	}
	if uerr := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped)); uerr != nil {
		slog.Debug("单实例锁探测：解锁失败，将随关闭释放", "err", uerr)
	}
	return false
}

// unlockInstance 释放锁并关闭文件（不显式 UnlockFileEx、不删除锁文件，理由同 unix 侧：
// 句柄关闭即释放锁）。
func unlockInstance(f *os.File) {
	if f == nil {
		return
	}
	if err := f.Close(); err != nil {
		slog.Warn("单实例锁：关闭锁文件失败，锁将随进程退出释放", "err", err)
	}
}
