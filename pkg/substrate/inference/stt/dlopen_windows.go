//go:build windows

package stt

import "syscall"

func dlopen(abs string) (uintptr, error) {
	h, err := syscall.LoadLibrary(abs)
	return uintptr(h), err
}
