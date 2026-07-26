package prompt

import (
	"fmt"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/security/taint"
	"github.com/polarisagi/polaris/pkg/types"
)

type PromptBuilder struct {
	zones [4][]types.Message
}

var _ protocol.PromptBuilder = (*PromptBuilder)(nil)

func NewPromptBuilder() *PromptBuilder {
	return &PromptBuilder{}
}

func (b *PromptBuilder) WriteInstruction(safe taint.SafeString) {
	b.zones[protocol.ZoneImmutable] = append(b.zones[protocol.ZoneImmutable], safe.IntoMessage("system"))
}

func (b *PromptBuilder) WriteSystemEnvironment(snapshot string) {
	b.zones[protocol.ZoneImmutable] = append(b.zones[protocol.ZoneImmutable], types.Message{
		Role:    "system",
		Content: snapshot,
	})
}

func (b *PromptBuilder) WriteCoreMemory(blocks []types.CoreMemoryBlock) {
	for _, block := range blocks {
		content := fmt.Sprintf("<core_memory block=\"%s\">\n%s\n</core_memory>", block.BlockKey, block.Content)
		if block.TaintLevel >= types.TaintHigh {
			content = taint.Spotlighting(taint.NewTaintedString(content, taint.TaintSource{OriginTaintLevel: block.TaintLevel}, "core_memory"))
		}

		b.zones[protocol.ZoneCoreMemory] = append(b.zones[protocol.ZoneCoreMemory], types.Message{
			Role:    "system",
			Content: content,
		})
	}
}

func (b *PromptBuilder) WriteUserData(ts taint.TaintedString) {
	b.zones[protocol.ZoneTaintedData] = append(b.zones[protocol.ZoneTaintedData], types.Message{
		Role:    "user",
		Content: taint.Spotlighting(ts),
	})
}

func (b *PromptBuilder) WriteUserImages(imgs []types.ImagePart) {
	if len(imgs) == 0 {
		return
	}
	parts := make([]any, 0, len(imgs))
	for _, img := range imgs {
		parts = append(parts, img)
	}
	b.zones[protocol.ZoneTaintedData] = append(b.zones[protocol.ZoneTaintedData], types.Message{
		Role:  "user",
		Parts: parts,
	})
}

func (b *PromptBuilder) Build() []types.Message {
	var result []types.Message //nolint:prealloc
	result = append(result, b.zones[protocol.ZoneImmutable]...)
	result = append(result, b.zones[protocol.ZoneCoreMemory]...)
	result = append(result, b.zones[protocol.ZoneMutableSkill]...)
	result = append(result, b.zones[protocol.ZoneTaintedData]...)
	return result
}

func (b *PromptBuilder) WriteComputerUsePolicy(mode string, anyAppEnabled, chromeEnabled bool) {
	if mode == "" {
		mode = "auto_review"
	}

	data := map[string]any{
		"Mode":          mode,
		"AnyAppEnabled": anyAppEnabled,
		"ChromeEnabled": chromeEnabled,
	}

	policy, err := configs.LoadPromptTemplate("kernel/computer_use_policy.md", data)
	if err != nil {
		policy = "Computer Use Confirmations Policy: mode=" + mode
	}

	b.zones[protocol.ZoneImmutable] = append(b.zones[protocol.ZoneImmutable], types.Message{
		Role:    "system",
		Content: policy,
	})
}

func (b *PromptBuilder) WriteToolHints(hint string) {
	if hint == "" {
		return
	}
	b.zones[protocol.ZoneImmutable] = append(b.zones[protocol.ZoneImmutable], types.Message{
		Role:    "system",
		Content: hint,
	})
}
