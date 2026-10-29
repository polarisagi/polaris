//go:build !windows

package ffi

import (
	"github.com/ebitengine/purego"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func dlopen(abs string) (uintptr, error) {
	h, err := purego.Dlopen(abs, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "ffi: dlopen 失败: "+abs, err)
	}
	return h, nil
}
