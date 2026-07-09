package plugin

import (
	"testing"

	"github.com/polarisagi/polaris/internal/protocol"
)

func TestParseSkillEntry(t *testing.T) {
	_, _ = parseSkillEntry("test", "test", protocol.Marketplace{})
}
