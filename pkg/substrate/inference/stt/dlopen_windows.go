//go:build windows

package stt

import "syscall"

// Dlopen 平台安全地加载动态库
func Dlopen(abs string) (uintptr, error) {
	h, err := syscall.LoadLibrary(abs)
	return uintptr(h), err
}
