package tool

import (
	"embed"
	"encoding/json"
	"fmt"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

//go:embed builtin
var builtinFS embed.FS

// toolYAML mirrors the tool.yaml schema (aligned with Anthropic agentskills.io format).
type toolYAML struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Version     string   `yaml:"version"`
	Capability  string   `yaml:"capability"`   // read-only | write-local | write-network | privileged
	RiskLevel   string   `yaml:"risk_level"`   // low | medium | high | privileged
	Sandbox     string   `yaml:"sandbox"`      // in-process | container | remote
	SideEffects []string `yaml:"side_effects"` // none | file-write | network-call | process-spawn | state-mutate
	Tags        []string `yaml:"tags"`
}

// LoadBuiltinToolMeta loads tool.yaml + schema.json from the embedded builtin/ directory
// and assembles a types.Tool ready for registration.
func LoadBuiltinToolMeta(name string) (types.Tool, error) {
	dir := filepath.Join("builtin", name)

	yamlBytes, err := builtinFS.ReadFile(filepath.Join(dir, "tool.yaml"))
	if err != nil {
		return types.Tool{}, apperr.Wrap(apperr.CodeInternal,
			fmt.Sprintf("loader: read tool.yaml for %q", name), err)
	}

	var meta toolYAML
	if err := yaml.Unmarshal(yamlBytes, &meta); err != nil {
		return types.Tool{}, apperr.Wrap(apperr.CodeInternal,
			fmt.Sprintf("loader: parse tool.yaml for %q", name), err)
	}

	var inputSchema map[string]any
	schemaBytes, err := builtinFS.ReadFile(filepath.Join(dir, "schema.json"))
	if err != nil {
		return types.Tool{}, apperr.Wrap(apperr.CodeInternal,
			fmt.Sprintf("loader: read schema.json for %q", name), err)
	}
	if err := json.Unmarshal(schemaBytes, &inputSchema); err != nil {
		return types.Tool{}, apperr.Wrap(apperr.CodeInternal,
			fmt.Sprintf("loader: parse schema.json for %q", name), err)
	}

	return types.Tool{
		Name:        meta.Name,
		Description: meta.Description,
		Version:     meta.Version,
		InputSchema: inputSchema,
		Capability:  mapCapability(meta.Capability),
		RiskLevel:   mapRiskLevel(meta.RiskLevel),
		SandboxTier: mapSandbox(meta.Sandbox),
		SideEffects: mapSideEffects(meta.SideEffects),
		Source:      types.ToolBuiltin,
		TrustTier:   types.TrustSystem,
	}, nil
}

func mapCapability(s string) types.CapabilityLevel {
	switch s {
	case "write-local":
		return types.CapWriteLocal
	case "write-network":
		return types.CapWriteNetwork
	case "privileged":
		return types.CapPrivileged
	default:
		return types.CapReadOnly
	}
}

func mapRiskLevel(s string) types.RiskLevel {
	switch s {
	case "medium":
		return types.RiskMedium
	case "high":
		return types.RiskHigh
	case "privileged":
		return types.RiskPrivileged
	default:
		return types.RiskLow
	}
}

func mapSandbox(s string) types.SandboxTier {
	switch s {
	case "container":
		return types.SandboxContainer
	case "remote":
		return types.SandboxRemote
	default:
		return types.SandboxInProcess
	}
}

func mapSideEffects(ss []string) []types.SideEffect {
	out := make([]types.SideEffect, 0, len(ss))
	for _, s := range ss {
		switch s {
		case "file-write":
			out = append(out, types.SideFileWrite)
		case "network-call":
			out = append(out, types.SideNetworkCall)
		case "process-spawn":
			out = append(out, types.SideProcessSpawn)
		case "state-mutate":
			out = append(out, types.SideStateMutate)
		default:
			out = append(out, types.SideNone)
		}
	}
	if len(out) == 0 {
		return []types.SideEffect{types.SideNone}
	}
	return out
}
