package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/polarisagi/polaris/internal/security/taint"

	"time"

	"github.com/polarisagi/polaris/internal/extension/llmgen"
	llmparent "github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

// skillStructuredBackoff 结构化 JSON 纠错重试退避（阶段03 R-06）：比
// llmparent.DefaultBackoff() 的默认 5s 基值更短——这里重试的目的是让模型
// 依据回灌的错误信息重新生成，不是规避 Provider 限流，长间隔只会拖慢
// 面向用户的交互式生成请求。
func skillStructuredBackoff() llmparent.BackoffConfig {
	return llmparent.BackoffConfig{Base: 500 * time.Millisecond, Max: 3 * time.Second, JitterRatio: 0.3}
}

var validNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// LLMClient is a minimal interface for the SkillCreator to generate responses.
type LLMClient interface {
	// Generate uses the system prompt and user intent to generate a structured response.
	Generate(ctx context.Context, systemPrompt, userPrompt string) (string, error)
	// GenerateJSON 语义同 Generate，但要求实现尽力让 Provider 侧强制返回合法
	// JSON（如 response_format=json_object）；不支持结构化输出的 Provider 可
	// 退化为等同于 Generate——上层 StructuredGenerator 的 extractJSON 兜底与
	// 有界重试仍然生效（阶段03 R-06）。
	GenerateJSON(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// ExtensionInstaller 是 SkillCreator 唯一需要的安装能力（消费方定义接口，
// HE-3/R1.4：接口在调用方定义），只声明本文件实际调用的 InstallExtension 一个
// 方法。2026-07-21 deadcode 审查补齐触发入口时，把此前要求的具体类型
// *marketplace.Manager 收窄为接口——sysadmin.SysAdminHandler.InstallMgr 早已是
// 结构上兼容的接口（额外多一个 Authorize 方法），无需在调用方再构造一个
// *marketplace.Manager 实例就能直接复用同一个已经完成安装策略校验的依赖。
type ExtensionInstaller interface {
	InstallExtension(ctx context.Context, req protocol.ExtensionInstallRequest) error
}

// SkillCreator defines the auto-generation workflow for skills based on Codex templates.
type SkillCreator struct {
	llm        LLMClient
	baseDir    string // e.g. ~/.polarisagi/polaris/plugins/user/
	installMgr ExtensionInstaller
	registry   protocol.SkillRegistry
	structGen  *llmgen.StructuredGenerator // 阶段03 R-06：有界重试+熔断+tracing/metrics
}

// NewSkillCreator initializes a new creator for auto-generating skills.
func NewSkillCreator(llm LLMClient, baseDir string, installMgr ExtensionInstaller, registry protocol.SkillRegistry) *SkillCreator {
	return &SkillCreator{
		llm:        llm,
		baseDir:    baseDir,
		installMgr: installMgr,
		registry:   registry,
		structGen:  llmgen.NewStructuredGenerator("skill").WithBackoff(skillStructuredBackoff()),
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

// parseGeneratedSkill 解析 LLM 生成的响应为结构化 GeneratedSkill 并校验必填字段
// （从 GenerateSkill 拆出，gocyclo 治理，行为不变）。
func parseGeneratedSkill(response string) (GeneratedSkill, error) {
	// Simple JSON extraction to handle model quirks
	jsonStr := extractJSON(response)

	var result GeneratedSkill
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		return GeneratedSkill{}, apperr.Wrap(apperr.CodeInternal, "skill_creator: failed to parse generated skill JSON", err)
	}

	if result.Name == "" || result.Description == "" {
		return GeneratedSkill{}, apperr.New(apperr.CodeInternal, "skill_creator: invalid generation, missing name or description")
	}

	if result.ExecMode == "" {
		result.ExecMode = "tool"
	}
	return result, nil
}

// writeSkillFiles 落盘物理插件目录结构（SKILL.md + plugin.json），返回 pluginDir
// （从 GenerateSkill 拆出，gocyclo 治理，行为不变）。
func writeSkillFiles(baseDir string, result GeneratedSkill) (string, error) {
	// Create physical directory structure
	if !validNamePattern.MatchString(result.Name) {
		return "", apperr.New(apperr.CodeInvalidInput, "skill_creator: invalid name")
	}

	pluginDir := filepath.Join(baseDir, result.Name)
	cleanBase := filepath.Clean(baseDir) + string(os.PathSeparator)
	cleanPluginDir := filepath.Clean(pluginDir)
	if !strings.HasPrefix(cleanPluginDir+string(os.PathSeparator), cleanBase) {
		return "", apperr.New(apperr.CodeInvalidInput, "skill_creator: path traversal detected")
	}
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
	return pluginDir, nil
}

// GenerateSkill takes a user's intent, calls the LLM, and creates the physical skill directory and SKILL.md.
func (c *SkillCreator) GenerateSkill(ctx context.Context, intent taint.TaintedString) (string, error) {
	if c.llm == nil {
		return "", apperr.New(apperr.CodeInternal, "skill_creator: LLM client is nil")
	}

	// 阶段03 R-06：优先走 GenerateJSON（Provider 侧结构化输出尽力保证合法 JSON），
	// 有界重试（含错误回灌）+ 熔断 + tracing/metrics 交由 StructuredGenerator
	// 统一处理；解析失败时把错误摘要回灌进下一次重试的 prompt。
	var result GeneratedSkill
	genErr := c.structGen.Generate(ctx, skillCreatorSystemPrompt, taint.Spotlighting(intent), c.llm.GenerateJSON, func(raw string) error {
		parsed, perr := parseGeneratedSkill(raw)
		if perr != nil {
			return perr
		}
		result = parsed
		return nil
	})
	if genErr != nil {
		// apperr.CodeOf 保留 llmgen 内部判定的具体 Code（如熔断开启时的
		// CodeResourceExhausted），不能用固定 Code 重新包装掩盖语义。
		return "", apperr.Wrap(apperr.CodeOf(genErr), "skill_creator: structured skill generation failed", genErr)
	}

	pluginDir, err := writeSkillFiles(c.baseDir, result)
	if err != nil {
		return "", err
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
