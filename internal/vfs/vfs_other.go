//go:build !unix

package vfs

import (
	"os"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// SafeOpen opens a file for reading. On non-unix platforms, it falls back to os.Open without O_NOFOLLOW.
func SafeOpen(name string) (*os.File, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "vfs: SafeOpen 打开文件失败: "+name, err)
	}
	return f, nil
}

// SafeOpenFile opens a file. On non-unix platforms, it falls back to os.OpenFile without O_NOFOLLOW.
func SafeOpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	f, err := os.OpenFile(name, flag, perm)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "vfs: SafeOpenFile 打开文件失败: "+name, err)
	}
	return f, nil
}
