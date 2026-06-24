//go:build unix

package vfs

import (
	"errors"
	"os"
	"syscall"

	"github.com/polarisagi/polaris/internal/observability/metrics"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// SafeOpen securely opens a file for reading, using O_NOFOLLOW to mitigate symlink attacks.
// ELOOP（符号链接越狱）→ InjectFaultSignal 提升路由级别 + 返回 CodeForbidden。
func SafeOpen(name string) (*os.File, error) {
	f, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil && errors.Is(err, syscall.ELOOP) {
		metrics.GlobalSurpriseIndex().InjectFaultSignal(0.5)
		return nil, apperr.New(apperr.CodeForbidden, "vfs: symlink traversal detected: "+name)
	}
	return f, err
}

// SafeOpenFile securely opens a file, ensuring O_NOFOLLOW is applied to mitigate symlink attacks.
// ELOOP（符号链接越狱）→ InjectFaultSignal 提升路由级别 + 返回 CodeForbidden。
func SafeOpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(name, flag|syscall.O_NOFOLLOW, perm)
	if err != nil && errors.Is(err, syscall.ELOOP) {
		metrics.GlobalSurpriseIndex().InjectFaultSignal(0.5)
		return nil, apperr.New(apperr.CodeForbidden, "vfs: symlink traversal detected: "+name)
	}
	return f, err
}
