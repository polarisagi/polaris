//go:build windows

package network

import (
	"github.com/polarisagi/polaris/pkg/apperr"
)

func (s *OSNetworkSandbox) enableOS() error {
	return apperr.New(apperr.CodeInternal,
		"local_only: windows OS-level network sandbox not implemented; use L2/L3 protection layers")
}
