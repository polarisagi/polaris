//go:build !darwin && !linux && !windows

package network

import (
	"fmt"

	"github.com/polarisagi/polaris/pkg/apperr"
)

func (s *OSNetworkSandbox) enableOS() error {
	return apperr.New(apperr.CodeInternal, fmt.Sprintf("local_only: unsupported platform %s", s.platform))
}
