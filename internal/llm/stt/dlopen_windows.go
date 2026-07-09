//go:build windows

package stt

import (
	"syscall"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// Dlopen 平台安全地加载动态库
func Dlopen(abs string) (uintptr, error) {
	h, err := syscall.LoadLibrary(abs)
	if err != nil {
		return uintptr(h), apperr.Wrap(apperr.CodeInternal, "Dlopen", err)
	}
	return uintptr(h), nil
}
