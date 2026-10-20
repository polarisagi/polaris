package native

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
	Capability  string   `yaml:"capability"`
	RiskLevel   string   `yaml:"risk_level"`
	Sandbox     string   `yaml:"sandbox"`
	SideEffects []string `yaml:"side_effects"`
	Tags        []string `yaml:"tags"`
}

// GetExtensionToolMeta loads tool.yaml + schema.json from the embedded builtin/ directory.
func GetExtensionToolMeta(name string) (types.Tool, error) {
	dir := filepath.Join("builtin", name)

	yamlBytes, err := builtinFS.ReadFile(filepath.Join(dir, "tool.yaml"))
	if err != nil {
		return types.Tool{}, apperr.Wrap(apperr.CodeInternal,
			fmt.Sprintf("native/loader: read tool.yaml for %q", name), err)
	}

	var meta toolYAML
	if err := yaml.Unmarshal(yamlBytes, &meta); err != nil {
		return types.Tool{}, apperr.Wrap(apperr.CodeInternal,
			fmt.Sprintf("native/loader: parse tool.yaml for %q", name), err)
	}

	var inputSchema map[string]any
	schemaBytes, err := builtinFS.ReadFile(filepath.Join(dir, "schema.json"))
	if err != nil {
		return types.Tool{}, apperr.Wrap(apperr.CodeInternal,
			fmt.Sprintf("native/loader: read schema.json for %q", name), err)
	}
	if err := json.Unmarshal(schemaBytes, &inputSchema); err != nil {
		return types.Tool{}, apperr.Wrap(apperr.CodeInternal,
			fmt.Sprintf("native/loader: parse schema.json for %q", name), err)
	}

	capLevel, err := mapCapability(meta.Capability)
	if err != nil {
		return types.Tool{}, apperr.Wrap(apperr.CodeOf(err), fmt.Sprintf("%s: tool %q", "native/loader", name), err)
	}
	riskLevel, err := mapRiskLevel(meta.RiskLevel)
	if err != nil {
		return types.Tool{}, apperr.Wrap(apperr.CodeOf(err), fmt.Sprintf("%s: tool %q", "native/loader", name), err)
	}

	return types.Tool{
		Name:        meta.Name,
		Description: meta.Description,
		Version:     meta.Version,
		InputSchema: inputSchema,
		Capability:  capLevel,
		RiskLevel:   riskLevel,
		SandboxTier: mapSandbox(meta.Sandbox),
		SideEffects: mapSideEffects(meta.SideEffects),
		Source:      types.ToolBuiltin,
		TrustTier:   types.TrustSystem,
	}, nil
}

// mapCapability 把 tool.yaml 的 capability 字面量映射为能力等级。
// fail-closed：未知/空值一律报错，不再默认只读（曾因 "CapWrite"/"write_local" 等无效值
// 静默降为只读，使写类工具绕过污点与审批，ADR-0117）。
func mapCapability(s string) (types.CapabilityLevel, error) {
	switch s {
	case "read-only":
		return types.CapReadOnly, nil
	case "read-network":
		return types.CapReadNetwork, nil
	case "write-local":
		return types.CapWriteLocal, nil
	case "write-network":
		return types.CapWriteNetwork, nil
	case "privileged":
		return types.CapPrivileged, nil
	default:
		return 0, apperr.New(apperr.CodeInvalidInput,
			fmt.Sprintf("%s: invalid capability %q (want read-only|read-network|write-local|write-network|privileged)", "native/loader", s))
	}
}

// mapRiskLevel 同样 fail-closed：缺失或非法的 risk_level 报错而非默认 low。
func mapRiskLevel(s string) (types.RiskLevel, error) {
	switch s {
	case "low":
		return types.RiskLow, nil
	case "medium":
		return types.RiskMedium, nil
	case "high":
		return types.RiskHigh, nil
	case "privileged":
		return types.RiskPrivileged, nil
	default:
		return 0, apperr.New(apperr.CodeInvalidInput,
			fmt.Sprintf("%s: invalid risk_level %q (want low|medium|high|privileged)", "native/loader", s))
	}
}

func mapSandbox(s string) types.SandboxTier {
	switch s {
	case "container":
		return types.SandboxContainer
	case "remote":
		return types.SandboxRemote
	case "wasm":
		return types.SandboxWasm
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
