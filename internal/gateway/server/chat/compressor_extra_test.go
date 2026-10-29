package chat

import (
	"testing"

	"github.com/polarisagi/polaris/internal/config"
)

func TestCompressorWarnPct(t *testing.T) {
	c := NewCompressionService(nil, nil, nil, config.CompressorConfig{}, nil, 0.0)
	pct := c.WarnPct()
	if pct != 80.0 {
		t.Errorf("expected 80.0, got %f", pct)
	}
}
