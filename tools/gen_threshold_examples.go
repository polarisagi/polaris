//go:build ignore
// +build ignore

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"

	"github.com/polarisagi/polaris/internal/config"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: go run gen_threshold_examples.go <output_dir>")
		os.Exit(1)
	}
	outDir := os.Args[1]

	if err := os.MkdirAll(outDir, 0755); err != nil {
		fmt.Printf("failed to create directory: %v\n", err)
		os.Exit(1)
	}

	t := config.DefaultThresholds()

	modules := map[string]interface{}{
		"m1_router.toml":        &t.M1Router,
		"m2_storage.toml":       &t.M2Storage,
		"m3_observability.toml": &t.M3Observability,
		"m4_kernel.toml":        &t.M4Kernel,
		"m5_memory.toml":        &t.M5Memory,
		"m6_skill.toml":         &t.M6Skill,
		"m7_tool.toml":          &t.M7Tool,
		"m8_orchestrator.toml":  &t.M8Orchestrator,
		"m9_self_improve.toml":  &t.M9SelfImprove,
		"m10_knowledge.toml":    &t.M10Knowledge,
		"m11_policy.toml":       &t.M11Policy,
		"m12_eval.toml":         &t.M12Eval,
		"m13_interface.toml":    &t.M13Interface,
	}

	for file, target := range modules {
		path := filepath.Join(outDir, file)
		data, err := toml.Marshal(target)
		if err != nil {
			fmt.Printf("failed to marshal %s: %v\n", file, err)
			os.Exit(1)
		}

		header := fmt.Sprintf("# Example threshold overrides for %s\n", file)
		header += "# To use, copy this file to ~/.polarisagi/polaris/config/ or point POLARIS_THRESHOLDS_DIR to this directory.\n\n"

		if err := os.WriteFile(path, append([]byte(header), data...), 0644); err != nil {
			fmt.Printf("failed to write %s: %v\n", file, err)
			os.Exit(1)
		}
		fmt.Printf("Generated %s\n", path)
	}
	fmt.Println("Successfully generated all threshold examples.")
}
