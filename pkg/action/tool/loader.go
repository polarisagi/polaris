package tool

import (
	"embed"
	"encoding/json"
	"fmt"
	"path/filepath"

	"gopkg.in/yaml.v3"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
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
// and assembles a protocol.Tool ready for registration.
func LoadBuiltinToolMeta(name string) (protocol.Tool, error) {
	dir := filepath.Join("builtin", name)

	yamlBytes, err := builtinFS.ReadFile(filepath.Join(dir, "tool.yaml"))
	if err != nil {
		return protocol.Tool{}, perrors.Wrap(perrors.CodeInternal,
			fmt.Sprintf("loader: read tool.yaml for %q", name), err)
	}

	var meta toolYAML
	if err := yaml.Unmarshal(yamlBytes, &meta); err != nil {
		return protocol.Tool{}, perrors.Wrap(perrors.CodeInternal,
			fmt.Sprintf("loader: parse tool.yaml for %q", name), err)
	}

	var inputSchema map[string]any
	schemaBytes, err := builtinFS.ReadFile(filepath.Join(dir, "schema.json"))
	if err != nil {
		return protocol.Tool{}, perrors.Wrap(perrors.CodeInternal,
			fmt.Sprintf("loader: read schema.json for %q", name), err)
	}
	if err := json.Unmarshal(schemaBytes, &inputSchema); err != nil {
		return protocol.Tool{}, perrors.Wrap(perrors.CodeInternal,
			fmt.Sprintf("loader: parse schema.json for %q", name), err)
	}

	return protocol.Tool{
		Name:        meta.Name,
		Description: meta.Description,
		Version:     meta.Version,
		InputSchema: inputSchema,
		Capability:  mapCapability(meta.Capability),
		RiskLevel:   mapRiskLevel(meta.RiskLevel),
		SandboxTier: mapSandbox(meta.Sandbox),
		SideEffects: mapSideEffects(meta.SideEffects),
		Source:      protocol.ToolBuiltin,
	}, nil
}

func mapCapability(s string) protocol.CapabilityLevel {
	switch s {
	case "write-local":
		return protocol.CapWriteLocal
	case "write-network":
		return protocol.CapWriteNetwork
	case "privileged":
		return protocol.CapPrivileged
	default:
		return protocol.CapReadOnly
	}
}

func mapRiskLevel(s string) protocol.RiskLevel {
	switch s {
	case "medium":
		return protocol.RiskMedium
	case "high":
		return protocol.RiskHigh
	case "privileged":
		return protocol.RiskPrivileged
	default:
		return protocol.RiskLow
	}
}

func mapSandbox(s string) protocol.SandboxTier {
	switch s {
	case "container":
		return protocol.SandboxContainer
	case "remote":
		return protocol.SandboxRemote
	default:
		return protocol.SandboxInProcess
	}
}

func mapSideEffects(ss []string) []protocol.SideEffect {
	out := make([]protocol.SideEffect, 0, len(ss))
	for _, s := range ss {
		switch s {
		case "file-write":
			out = append(out, protocol.SideFileWrite)
		case "network-call":
			out = append(out, protocol.SideNetworkCall)
		case "process-spawn":
			out = append(out, protocol.SideProcessSpawn)
		case "state-mutate":
			out = append(out, protocol.SideStateMutate)
		default:
			out = append(out, protocol.SideNone)
		}
	}
	if len(out) == 0 {
		return []protocol.SideEffect{protocol.SideNone}
	}
	return out
}
