//go:build ignore

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/config"
)

func main() {
	manifest := make(map[string]string)
	for _, dir := range config.ImmutableKernelPackages() {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if !info.IsDir() && filepath.Ext(path) == ".go" {
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				defer f.Close()
				h := sha256.New()
				if _, err := io.Copy(h, f); err != nil {
					return err
				}
				manifest[path] = hex.EncodeToString(h.Sum(nil))
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "Error walking %s: %v\n", dir, err)
			os.Exit(1)
		}
	}

	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling manifest: %v\n", err)
		os.Exit(1)
	}

	outPath := "internal/config/kernel_manifest.json"
	if err := os.WriteFile(outPath, b, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing manifest: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Generated %s\n", outPath)
}
