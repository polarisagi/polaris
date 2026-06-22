//go:build windows

package stt

import (
	"fmt"
	"syscall"
)

// Dlopen 平台安全地加载动态库
func Dlopen(abs string) (uintptr, error) {
	h, err := syscall.LoadLibrary(abs)
	if err != nil {
		return uintptr(h), fmt.Errorf("Dlopen: %w", err)
	}
	return uintptr(h), nil
}
