//go:build windows

package ffi

import "syscall"

func dlopen(abs string) (uintptr, error) {
	h, err := syscall.LoadLibrary(abs)
	return uintptr(h), err
}
