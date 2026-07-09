package sys_probe

import (
	"context"
	"encoding/json"

	"github.com/polarisagi/polaris/internal/sysinfo"
)

func SysProbeFn(_ context.Context, _ []byte) ([]byte, error) {
	info := sysinfo.GetSystemInfo()
	return json.Marshal(info) //nolint:wrapcheck
}
