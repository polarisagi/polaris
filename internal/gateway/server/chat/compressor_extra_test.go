package chat

import (
	"testing"

	"github.com/polarisagi/polaris/internal/config"
)

func TestCompressorWarnPct(t *testing.T) {
	c := NewCompressor(nil, nil, nil, config.CompressorConfig{})
	pct := c.WarnPct()
	if pct != 80.0 {
		t.Errorf("expected 80.0, got %f", pct)
	}
}
