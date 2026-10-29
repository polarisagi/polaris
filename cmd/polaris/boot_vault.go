package main

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/config"
)

// resolveVaultDirs 展开并过滤 knowledge.vault_dirs：~ 展开、Clean、去重、跳过与工作区重复和不存在的目录。
// 资料库目录由插件按需创建，缺失属常态，只记 Info 不报错（ADR-0114 决策八）。
func resolveVaultDirs(dirs []string, workspace string) []string {
	seen := map[string]bool{filepath.Clean(workspace): true}
	var out []string
	for _, raw := range dirs {
		if raw == "" {
			continue
		}
		if raw == "~" {
			raw = "~/"
		}
		dir := filepath.Clean(config.ExpandHome(raw))
		if seen[dir] {
			slog.Info("polaris: vault dir duplicates an already synced dir, skipped", "dir", dir)
			continue
		}
		seen[dir] = true
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			slog.Info("polaris: vault dir not found, skipped", "dir", dir)
			continue
		}
		out = append(out, dir)
	}
	return out
}
