//go:build !windows

package stt

import "github.com/ebitengine/purego"

// Dlopen 平台安全地加载动态库
func Dlopen(abs string) (uintptr, error) {
	return purego.Dlopen(abs, purego.RTLD_NOW|purego.RTLD_GLOBAL)
}
