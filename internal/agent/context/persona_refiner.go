package agentctx

// PersonaRefiner — 用户画像精炼器（M05 §2.3 简化版）
// 设计决策: 原规范 11 维度对自托管场景过度设计，简化为 5 个实用维度。
// 删除: ColdStartManager、ProactiveQuery（无生产证明价值）。
// 持久化: preferences 表单条 JSON（key="user_profile"），无跨用户隔离需求。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// UserProfile 5 维用户画像。
// 写路径: PersonaRefiner.Update
// 读路径: PreferencesBlock → 内核会话层快照（ADR-0120 决策二）
type UserProfile struct {
	// LanguagePref 用户首选语言，影响回复语言选择。
	LanguagePref string `json:"language_pref"` // zh-CN | en | mixed
	// ResponseStyle 回复风格偏好。
	ResponseStyle string `json:"response_style"` // concise | detailed | casual | formal
	// OutputFormat 输出格式偏好。
	OutputFormat string `json:"output_format"` // markdown | plain | code-first
	// Expertise 领域专业程度（影响解释深度和术语使用）。
	Expertise string `json:"expertise"` // novice | intermediate | expert
	// InteractionSummary 历史字段：曾由每回合一次的 LLM 调用生成并注入稳定层，ADR-0120 起
	// 不再生成也不再注入；保留字段只为兼容已持久化的 JSON。
	InteractionSummary string `json:"interaction_summary"`
	// UpdatedAt Unix 秒，记录最近更新时间。
	UpdatedAt int64 `json:"updated_at"`
}

const profilePreferenceKey = "user_profile"

// PersonaRefiner 从会话信号中精炼用户画像，跨会话持久化。
type PersonaRefiner struct {
	db      protocol.SQLQuerier
	mu      sync.Mutex
	profile *UserProfile
}

// NewPersonaRefiner 创建 PersonaRefiner。
func NewPersonaRefiner(db protocol.SQLQuerier) *PersonaRefiner {
	return &PersonaRefiner{
		db:      db,
		profile: defaultUserProfile(),
	}
}

func defaultUserProfile() *UserProfile {
	return &UserProfile{
		LanguagePref:  "zh-CN",
		ResponseStyle: "concise",
		OutputFormat:  "markdown",
		Expertise:     "intermediate",
	}
}

// Load 从 preferences 表加载用户画像；不存在时保留默认值（冷启动场景）。
func (pr *PersonaRefiner) Load(ctx context.Context) error {
	if pr.db == nil {
		return nil
	}
	row := pr.db.QueryRowContext(ctx,
		"SELECT value FROM preferences WHERE key = ?", profilePreferenceKey)
	var raw string
	if err := row.Scan(&raw); errors.Is(err, sql.ErrNoRows) {
		return nil // 冷启动，保持默认
	} else if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "PersonaRefiner.Load", err)
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if err := json.Unmarshal([]byte(raw), pr.profile); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "PersonaRefiner.Load 反序列化失败", err)
	}
	return nil
}

// Save 将用户画像持久化到 preferences 表。
func (pr *PersonaRefiner) Save(ctx context.Context) error {
	if pr.db == nil {
		return nil
	}
	pr.mu.Lock()
	pr.profile.UpdatedAt = time.Now().Unix()
	raw, err := json.Marshal(pr.profile)
	pr.mu.Unlock()
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "PersonaRefiner.Save", err)
	}
	_, err = pr.db.ExecContext(ctx, `
		INSERT INTO preferences (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, profilePreferenceKey, string(raw))
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "PersonaRefiner.Save", err)
	}
	return nil
}

// Update 按显式信号更新画像维度（调用方在收到用户反馈时调用）。
// signals key 为维度名（language_pref / response_style / output_format / expertise），
// value 为新的偏好值。未知 key 静默忽略。
func (pr *PersonaRefiner) Update(signals map[string]string) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	for k, v := range signals {
		switch k {
		case "language_pref":
			pr.profile.LanguagePref = v
		case "response_style":
			pr.profile.ResponseStyle = v
		case "output_format":
			pr.profile.OutputFormat = v
		case "expertise":
			pr.profile.Expertise = v
		}
	}
}

// Profile 返回当前用户画像的快照（只读副本）。
func (pr *PersonaRefiner) Profile() UserProfile {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return *pr.profile
}

// PreferencesBlock 渲染四个稳定维度，供内核会话层在会话开始时快照一次（ADR-0120 决策二）。
// 不含 InteractionSummary：它曾每回合由 LLM 重写并注入稳定层，使每个新回合的前缀缓存全部失效。
// 维度顺序固定，空值跳过；全部为空返回 ""。
func (pr *PersonaRefiner) PreferencesBlock() string {
	pr.mu.Lock()
	p := *pr.profile
	pr.mu.Unlock()
	var lines []string
	for _, kv := range [][2]string{
		{"language_pref", p.LanguagePref},
		{"response_style", p.ResponseStyle},
		{"output_format", p.OutputFormat},
		{"expertise", p.Expertise},
	} {
		if v := strings.TrimSpace(kv[1]); v != "" {
			lines = append(lines, "- "+kv[0]+": "+v)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "## Learned user preferences\nDefaults inferred from past sessions; the user's words in this conversation override them.\n" + strings.Join(lines, "\n")
}
