//go:build windows

package embedonnx

import (
	"syscall"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func dlopen(abs string) (uintptr, error) {
	h, err := syscall.LoadLibrary(abs)
	if err != nil {
		return uintptr(h), apperr.Wrap(apperr.CodeInternal, "embedonnx: dlopen failed: "+abs+"（请检查 DLL 架构及其运行时依赖）", err)
	}
	return uintptr(h), nil
}
