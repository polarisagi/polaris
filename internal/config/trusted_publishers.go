package config

import (
	"io/fs"
	"log/slog"

	"gopkg.in/yaml.v3"
)

type TrustedPublisher struct {
	ID           string `yaml:"id"`
	DisplayName  string `yaml:"display_name"`
	Homepage     string `yaml:"homepage"`
	TrustTier    int    `yaml:"trust_tier"`
	ApprovalMode string `yaml:"approval_mode"`
	Description  string `yaml:"description"`
}

type TrustedPublishersConfig struct {
	Publishers []TrustedPublisher `yaml:"publishers"`
}

// LoadTrustedPublishers parses the trusted-publishers.yaml and returns a map
// of publisher ID to their assigned trust tier.
func LoadTrustedPublishers(fsys fs.FS, filepath string) map[string]int {
	m := make(map[string]int)
	b, err := fs.ReadFile(fsys, filepath)
	if err != nil {
		slog.Warn("config: failed to read trusted publishers", "file", filepath, "err", err)
		return m
	}

	var cfg TrustedPublishersConfig
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		slog.Warn("config: failed to parse trusted publishers yaml", "err", err)
		return m
	}

	for _, p := range cfg.Publishers {
		if p.ID != "" {
			m[p.ID] = p.TrustTier
		}
	}
	return m
}
