package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"time"

	"github.com/polarisagi/polaris/internal/extension/marketplace"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// LLMClient is a minimal interface for the SkillCreator to generate responses.
type LLMClient interface {
	// Generate uses the system prompt and user intent to generate a structured response.
	Generate(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// SkillCreator defines the auto-generation workflow for skills based on Codex templates.
type SkillCreator struct {
	llm        LLMClient
	baseDir    string // e.g. ~/.polarisagi/polaris/plugins/user/
	installMgr *marketplace.Manager
	registry   protocol.SkillRegistry
}

// NewSkillCreator initializes a new creator for auto-generating skills.
func NewSkillCreator(llm LLMClient, baseDir string, installMgr *marketplace.Manager, registry protocol.SkillRegistry) *SkillCreator {
	return &SkillCreator{
		llm:        llm,
		baseDir:    baseDir,
		installMgr: installMgr,
		registry:   registry,
	}
}

// GeneratedSkill represents the structured output expected from the LLM.
type GeneratedSkill struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Instructions string `json:"instructions"`
	ExecMode     string `json:"exec_mode"`
}

const skillCreatorSystemPrompt = `
You are the internal skill-creator agent. Your job is to translate a user's workflow description into a standard SKILL.md format.
A skill MUST have a concise name (kebab-case) and a clear description (what it does and when it should trigger) for progressive disclosure.

Output ONLY valid JSON matching this schema:
{
  "name": "skill-name",
  "description": "Trigger this skill when...",
  "instructions": "The detailed workflow steps...",
  "exec_mode": "tool"
}
Do not include any Markdown wrappers like ` + "```json" + ` in the output.
`

// GenerateSkill takes a user's intent, calls the LLM, and creates the physical skill directory and SKILL.md.
func (c *SkillCreator) GenerateSkill(ctx context.Context, intent string) (string, error) {
	if c.llm == nil {
		return "", apperr.New(apperr.CodeInternal, "skill_creator: LLM client is nil")
	}

	response, err := c.llm.Generate(ctx, skillCreatorSystemPrompt, intent)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "skill_creator: failed to generate skill", err)
	}

	// Simple JSON extraction to handle model quirks
	jsonStr := extractJSON(response)

	var result GeneratedSkill
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "skill_creator: failed to parse generated skill JSON", err)
	}

	if result.Name == "" || result.Description == "" {
		return "", apperr.New(apperr.CodeInternal, "skill_creator: invalid generation, missing name or description")
	}

	if result.ExecMode == "" {
		result.ExecMode = "tool"
	}

	// Create physical directory structure
	pluginDir := filepath.Join(c.baseDir, result.Name)
	skillsDir := filepath.Join(pluginDir, "skills", result.Name)

	if err := os.MkdirAll(skillsDir, 0755); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "skill_creator: failed to create skill directory", err)
	}

	// Write SKILL.md
	skillContent := fmt.Sprintf("---\nname: %s\ndescription: %s\nexec_mode: %s\n---\n\n%s\n", result.Name, result.Description, result.ExecMode, result.Instructions)
	skillPath := filepath.Join(skillsDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(skillContent), 0644); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "skill_creator: failed to write SKILL.md", err)
	}

	// Create a default plugin.json
	pluginMetaDir := filepath.Join(pluginDir, ".polaris-plugin")
	if err := os.MkdirAll(pluginMetaDir, 0755); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "skill_creator: failed to create .polaris-plugin directory", err)
	}

	pluginJSON := fmt.Sprintf(`{
  "name": "%s",
  "version": "1.0.0",
  "description": "%s",
  "skills": "./skills/"
}`, result.Name, result.Description)

	pluginJSONPath := filepath.Join(pluginMetaDir, "plugin.json")
	if err := os.WriteFile(pluginJSONPath, []byte(pluginJSON), 0644); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "skill_creator: failed to write plugin.json", err)
	}

	// Trigger security gate / DB registration via InstallExtension
	if c.installMgr == nil {
		return "", apperr.New(apperr.CodeInternal,
			"skill_creator: security manager not initialized, refusing to install (fail-closed)")
	}
	extID := "ext_llm_" + fmt.Sprintf("%d", time.Now().UnixNano())
	installReq := protocol.ExtensionInstallRequest{
		Principal:   "llm_agent",
		ExtensionID: extID,
		Name:        result.Name,
		ExtType:     "skill",
		TrustTier:   1, // TrustLocal
		Publisher:   "agent",
		HasHooks:    false,
		LocalPath:   pluginDir,
		RuntimeID:   result.Name,
	}
	if err := c.installMgr.InstallExtension(ctx, installReq); err != nil {
		_ = os.RemoveAll(pluginDir) // rollback file writes
		return "", apperr.Wrap(apperr.CodeForbidden, "skill_creator: installation blocked by policy gate", err)
	}

	if c.registry != nil {
		meta := types.SkillMeta{
			Name:      types.SkillPrefix + result.Name,
			Version:   "1.0.0",
			ExecMode:  result.ExecMode,
			Trust:     types.TrustLocal,
			RiskLevel: "low",
		}
		if err := c.registry.Register(ctx, meta); err != nil {
			return "", apperr.Wrap(apperr.CodeInternal, "skill_creator: failed to register skill in db", err)
		}
	}

	return pluginDir, nil
}

var jsonExtractRegex = regexp.MustCompile(`(?s)\{.*\}`)

func extractJSON(input string) string {
	match := jsonExtractRegex.FindString(input)
	if match != "" {
		return match
	}
	return input
}
