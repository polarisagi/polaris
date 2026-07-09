//go:build darwin

package network

import (
	"github.com/polarisagi/polaris/pkg/apperr"
)

func (s *OSNetworkSandbox) enableOS() error {
	// macOS sandbox-exec 仅能 wrap 新进程，无法限制当前运行中的 Go 进程。
	// sandbox_init() C API 需 CGO（ADR-0011 禁止 CGO）。
	// macOS L1 OS sandbox 不可实现；local_only 模式依赖 L2 NoopTransport + L3 SafeDialer。
	return apperr.New(apperr.CodeInternal,
		"local_only: macOS L1 OS sandbox not supported (sandbox_init requires CGO, disabled by ADR-0011); L2/L3 protection active")
}
