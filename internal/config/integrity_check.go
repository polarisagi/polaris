package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	_ "embed"

	"github.com/polarisagi/polaris/pkg/apperr"
)

//go:embed kernel_manifest.json
var kernelManifestJSON []byte

// VerifyKernelIntegrity checks the SHA-256 hashes of immutable kernel packages against the embedded manifest.
// If there is a mismatch or a file is missing/added, it returns an error.
func VerifyKernelIntegrity() error {
	var manifest map[string]string
	if err := json.Unmarshal(kernelManifestJSON, &manifest); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "failed to unmarshal kernel manifest", err)
	}

	currentManifest := make(map[string]string)
	releaseMode := false

	for _, dir := range ImmutableKernelPackages() {
		// 如果核心源码目录不存在，说明是作为 Release 发布的独立二进制运行，而非源码运行模式
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			// 源码目录不存在，进入 Release 二进制校验模式
			releaseMode = true
			break
		}

		if err := hashPackageDir(dir, currentManifest); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "VerifyKernelIntegrity", err)
		}
	}

	if releaseMode {
		// Release 模式：校验二进制自身哈希
		return verifyBinarySeal()
	}

	// Verify all expected files are present and match
	for path, expectedHash := range manifest {
		actualHash, ok := currentManifest[path]
		if !ok {
			return apperr.New(apperr.CodeInternal, fmt.Sprintf("integrity violation: missing immutable kernel file %s", path))
		}
		if actualHash != expectedHash {
			return apperr.New(apperr.CodeInternal, fmt.Sprintf("integrity violation: hash mismatch for %s (expected %s, got %s)", path, expectedHash, actualHash))
		}
	}

	// Verify no new unexpected files are present
	for path := range currentManifest {
		if _, ok := manifest[path]; !ok {
			return apperr.New(apperr.CodeInternal, fmt.Sprintf("integrity violation: unexpected new file in immutable kernel package %s", path))
		}
	}

	return nil
}

// verifyBinarySeal 计算当前可执行文件的 SHA-256，与附加的 .sha256 封印文件比对。
// 无封印文件时（开发构建未生成 sidecar），仅放行。
func verifyBinarySeal() error {
	exe, err := os.Executable()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "binary seal: cannot resolve executable path", err)
	}
	sidecar := exe + ".sha256"
	data, err := os.ReadFile(sidecar)
	if os.IsNotExist(err) {
		// 无封印文件：开发或未封印构建，打印警告后放行
		return nil
	}
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "binary seal: cannot read .sha256 sidecar", err)
	}
	expected := strings.TrimSpace(string(data))

	f, err := os.Open(exe)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "binary seal: cannot open executable", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "binary seal: hash read failed", err)
	}
	actual := hex.EncodeToString(h.Sum(nil))
	if actual != expected {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf(
			"CRITICAL: binary seal mismatch (expected %s, got %s)", expected, actual))
	}
	return nil
}

func hashPackageDir(dir string, currentManifest map[string]string) error {
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "hashPackageDir", err)
		}
		if !info.IsDir() && filepath.Ext(path) == ".go" {
			f, err := os.Open(path)
			if err != nil {
				return apperr.Wrap(apperr.CodeInternal, "hashPackageDir", err)
			}
			defer f.Close()
			h := sha256.New()
			if _, err := io.Copy(h, f); err != nil {
				return apperr.Wrap(apperr.CodeInternal, "hashPackageDir", err)
			}
			currentManifest[path] = hex.EncodeToString(h.Sum(nil))
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("failed to walk immutable package %s", dir), err)
	}
	return nil
}
