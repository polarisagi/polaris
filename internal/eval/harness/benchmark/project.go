package benchmark

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type ProjectAdapter struct{}

func (ProjectAdapter) Name() string { return "project" }

func (ProjectAdapter) Load(_ context.Context, datasetPath string) ([]protocol.EvalCase, error) {
	data, err := readBoundedDataset(datasetPath)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "project benchmark: read dataset", err)
	}
	var cases []protocol.EvalCase
	if err := json.Unmarshal(data, &cases); err != nil {
		return nil, apperr.Wrap(apperr.CodeInvalidInput, "project benchmark: parse EvalCase array", err)
	}
	for i := range cases {
		if err := validateProjectCase(cases[i]); err != nil {
			return nil, apperr.Wrap(apperr.CodeInvalidInput, "project benchmark: unsupported case "+cases[i].ID, err)
		}
	}
	return cases, nil
}

func validateProjectCase(c protocol.EvalCase) error {
	if err := validateProjectCaseIdentity(c); err != nil {
		return err
	}
	if err := validateProjectRepository(c); err != nil {
		return err
	}
	if err := validateProjectSource(c); err != nil {
		return err
	}
	if err := validateProjectCommands(c); err != nil {
		return err
	}
	if _, exists := c.Expected["patch"]; exists {
		return apperr.New(apperr.CodeInvalidInput, "gold patch must not be supplied in Expected")
	}
	return nil
}

func validateProjectCaseIdentity(c protocol.EvalCase) error {
	if c.ID == "" || c.BehaviorType != protocol.BehaviorCodePatch || strings.TrimSpace(c.Description) == "" {
		return apperr.New(apperr.CodeInvalidInput, "id, code_patch behavior, and problem statement are required")
	}
	return nil
}

func validateProjectRepository(c protocol.EvalCase) error {
	path, _ := c.Input["repo_path"].(string)
	commit, _ := c.Input["base_commit"].(string)
	if path == "" || commit == "" {
		return apperr.New(apperr.CodeUnimplemented, "unsupported: local repo_path and full base_commit are required")
	}
	if !filepath.IsAbs(path) || !fullCommitHash(commit) {
		return apperr.New(apperr.CodeInvalidInput, "repo_path must be absolute and base_commit must be a full hash")
	}
	return nil
}

func validateProjectSource(c protocol.EvalCase) error {
	sourceFiles, ok := c.Input["source_files"].(map[string]any)
	if !ok {
		return apperr.New(apperr.CodeUnimplemented, "unsupported: bounded source_files context is required")
	}
	sourceBytes, err := json.Marshal(sourceFiles)
	if err != nil || len(sourceFiles) == 0 || len(sourceBytes) > 256*1024 || !sourceFilesAreText(sourceFiles) {
		return apperr.New(apperr.CodeInvalidInput, "source_files must be non-empty and at most 256 KiB")
	}
	if testPatch, exists := c.Input["test_patch"]; exists {
		if _, ok := testPatch.(string); !ok {
			return apperr.New(apperr.CodeInvalidInput, "test_patch must be a string")
		}
	}
	return nil
}

func validateProjectCommands(c protocol.EvalCase) error {
	if !hasStringList(c.Config["fail_to_pass_commands"]) || !hasStringList(c.Config["pass_to_pass_commands"]) {
		return apperr.New(apperr.CodeUnimplemented, "unsupported: explicit fail_to_pass_commands and pass_to_pass_commands are required")
	}
	return nil
}

func fullCommitHash(commit string) bool {
	if (len(commit) != 40 && len(commit) != 64) || strings.ToLower(commit) != commit {
		return false
	}
	_, err := hex.DecodeString(commit)
	return err == nil
}

func sourceFilesAreText(sourceFiles map[string]any) bool {
	for path, content := range sourceFiles {
		text, ok := content.(string)
		clean := filepath.Clean(path)
		if !ok || path == "" || filepath.IsAbs(path) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || strings.Contains(path, "\\") || len(text) == 0 {
			return false
		}
	}
	return true
}

func hasStringList(value any) bool {
	switch items := value.(type) {
	case []any:
		if len(items) == 0 || len(items) > 8 {
			return false
		}
		for _, item := range items {
			if text, ok := item.(string); !ok || strings.TrimSpace(text) == "" {
				return false
			}
		}
		return true
	case []string:
		if len(items) == 0 || len(items) > 8 {
			return false
		}
		for _, item := range items {
			if strings.TrimSpace(item) == "" {
				return false
			}
		}
		return true
	default:
		return false
	}
}
