package prompt

import "github.com/polarisagi/polaris/internal/protocol"

const (
	ZoneImmutable    = protocol.ZoneImmutable
	ZoneCoreMemory   = protocol.ZoneCoreMemory
	ZoneMutableSkill = protocol.ZoneMutableSkill
	ZoneTaintedData  = protocol.ZoneTaintedData
)

// PromptBuilder alias to protocol.PromptBuilder to prevent breaking changes.
type PromptBuilder = protocol.PromptBuilder

// NewPromptBuilder alias to protocol.NewPromptBuilder
func NewPromptBuilder() *PromptBuilder {
	return protocol.NewPromptBuilder()
}
