//go:build !windows

package stt

import (
	"github.com/ebitengine/purego"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// Dlopen 平台安全地加载动态库
func Dlopen(abs string) (uintptr, error) {
	h, err := purego.Dlopen(abs, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "stt: dlopen 失败: "+abs, err)
	}
	return h, nil
}
