//go:build !unix

package vfs

import "os"

// SafeOpen opens a file for reading. On non-unix platforms, it falls back to os.Open without O_NOFOLLOW.
func SafeOpen(name string) (*os.File, error) {
	return os.Open(name)
}

// SafeOpenFile opens a file. On non-unix platforms, it falls back to os.OpenFile without O_NOFOLLOW.
func SafeOpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(name, flag, perm)
}
