//go:build windows

package ffi

import (
	"syscall"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func dlopen(abs string) (uintptr, error) {
	h, err := syscall.LoadLibrary(abs)
	if err != nil {
		return uintptr(h), apperr.Wrap(apperr.CodeInternal, "dlopen", err)
	}
	return uintptr(h), nil
}
