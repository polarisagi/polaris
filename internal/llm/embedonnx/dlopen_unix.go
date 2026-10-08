//go:build !windows

package embedonnx

import (
	"github.com/ebitengine/purego"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func dlopen(abs string) (uintptr, error) {
	h, err := purego.Dlopen(abs, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return 0, apperr.Wrap(apperr.CodeInternal, "embedonnx: dlopen failed", err)
	}
	return h, nil
}
