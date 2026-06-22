package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// AgentProfile 用户定义的自定义 Agent 配置（ADR-0015 §2.4）。
// 从 .polaris/agents/*.yaml 或 ~/.polarisagi/polaris/agents/*.yaml 加载。
// 映射到 AgentCard，在 Blackboard 中注册。
//
// 对应 Codex .codex/agents/*.toml，Polaris 使用 YAML 与整体配置惯例一致。
type AgentProfile struct {
	Name         string   `yaml:"name"`
	Description  string   `yaml:"description"`
	Instructions string   `yaml:"instructions"` // developer_instructions 对应
	Model        string   `yaml:"model,omitempty"`
	SandboxTier  int      `yaml:"sandbox_tier,omitempty"` // 0=继承父, 1/2/3 覆盖
	MaxDepth     int      `yaml:"max_depth,omitempty"`    // 默认 1，防递归
	MaxThreads   int      `yaml:"max_threads,omitempty"`  // 0=继承全局配置
	Skills       []string `yaml:"skills,omitempty"`
	MCPServers   []string `yaml:"mcp_servers,omitempty"`
}

// ToAgentCard 将 AgentProfile 转换为 AgentCard（Blackboard 注册格式）。
func (p *AgentProfile) ToAgentCard() AgentCard {
	tier := p.SandboxTier
	if tier == 0 {
		tier = 1 // 默认 Sbx-L1（read-only 等效）
	}
	return AgentCard{
		Name:        p.Name,
		Version:     "1.0.0",
		Description: p.Description,
		Skills:      p.Skills,
		Tools:       p.MCPServers,
		TrustLevel:  3, // 用户定义 Agent 默认信任级别
		SandboxTier: tier,
		MaxDepth:    p.MaxDepth,
	}
}

// LoadAgentProfiles 从目录扫描所有 *.yaml 加载 AgentProfile 列表。
func LoadAgentProfiles(dir string) ([]AgentProfile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("agent_profile: scan %s: %v", dir, err), err)
	}

	profiles := make([]AgentProfile, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		p, err := loadProfile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("LoadAgentProfiles: %w", err)
		}
		if err := validateProfile(p); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("agent_profile: %s: %v", e.Name(), err), err)
		}
		profiles = append(profiles, *p)
	}
	return profiles, nil
}

// DefaultAgentProfilePaths 返回惯例扫描路径（用户级 + 项目级）。
func DefaultAgentProfilePaths() []string {
	home, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	return []string{
		filepath.Join(home, ".polarisagi/polaris", "agents"),
		filepath.Join(cwd, ".polaris", "agents"),
	}
}

func loadProfile(path string) (*AgentProfile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("agent_profile: read %s: %v", path, err), err)
	}
	var p AgentProfile
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("agent_profile: parse %s: %v", path, err), err)
	}
	return &p, nil
}

func validateProfile(p *AgentProfile) error {
	if p.Name == "" {
		return apperr.New(apperr.CodeInternal, "name is required")
	}
	if p.Description == "" {
		return apperr.New(apperr.CodeInternal, "description is required")
	}
	if p.Instructions == "" {
		return apperr.New(apperr.CodeInternal, "instructions is required")
	}
	if p.MaxDepth < 0 {
		return apperr.New(apperr.CodeInternal, "max_depth must be >= 0")
	}
	return nil
}
