package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	_ "embed"

	"github.com/polarisagi/polaris/internal/errors"
)

//go:embed kernel_manifest.json
var kernelManifestJSON []byte

// VerifyKernelIntegrity checks the SHA-256 hashes of immutable kernel packages against the embedded manifest.
// If there is a mismatch or a file is missing/added, it returns an error.
func VerifyKernelIntegrity() error {
	var manifest map[string]string
	if err := json.Unmarshal(kernelManifestJSON, &manifest); err != nil {
		return errors.Wrap(errors.CodeInternal, "failed to unmarshal kernel manifest", err)
	}

	currentManifest := make(map[string]string)
	for _, dir := range ImmutableKernelPackages() {
		// 如果核心源码目录不存在，说明是作为 Release 发布的独立二进制运行，而非源码运行模式
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			// 在独立的二进制运行模式下，跳过源码级别的完整性校验
			return nil
		}

		if err := hashPackageDir(dir, currentManifest); err != nil {
			return err
		}
	}

	// Verify all expected files are present and match
	for path, expectedHash := range manifest {
		actualHash, ok := currentManifest[path]
		if !ok {
			return errors.New(errors.CodeInternal, fmt.Sprintf("integrity violation: missing immutable kernel file %s", path))
		}
		if actualHash != expectedHash {
			return errors.New(errors.CodeInternal, fmt.Sprintf("integrity violation: hash mismatch for %s (expected %s, got %s)", path, expectedHash, actualHash))
		}
	}

	// Verify no new unexpected files are present
	for path := range currentManifest {
		if _, ok := manifest[path]; !ok {
			return errors.New(errors.CodeInternal, fmt.Sprintf("integrity violation: unexpected new file in immutable kernel package %s", path))
		}
	}

	return nil
}

func hashPackageDir(dir string, currentManifest map[string]string) error {
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
			currentManifest[path] = hex.EncodeToString(h.Sum(nil))
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return errors.Wrap(errors.CodeInternal, fmt.Sprintf("failed to walk immutable package %s", dir), err)
	}
	return nil
}
