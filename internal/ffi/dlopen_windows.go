//go:build windows

package ffi

import (
	"fmt"
	"syscall"
)

func dlopen(abs string) (uintptr, error) {
	h, err := syscall.LoadLibrary(abs)
	if err != nil {
		return uintptr(h), fmt.Errorf("dlopen: %w", err)
	}
	return uintptr(h), nil
}
