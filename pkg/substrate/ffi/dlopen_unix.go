//go:build !windows

package ffi

import "github.com/ebitengine/purego"

func dlopen(abs string) (uintptr, error) {
	return purego.Dlopen(abs, purego.RTLD_NOW|purego.RTLD_GLOBAL)
}
