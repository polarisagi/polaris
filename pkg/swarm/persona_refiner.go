package swarm

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
)

// UserProfile 5 维用户画像。
// 写路径: PersonaRefiner.Update / RefineAtSessionEnd
// 读路径: ToUserPreferences → ImmutableCore.UserPreferences 注入
type UserProfile struct {
	// LanguagePref 用户首选语言，影响回复语言选择。
	LanguagePref string `json:"language_pref"` // zh-CN | en | mixed
	// ResponseStyle 回复风格偏好。
	ResponseStyle string `json:"response_style"` // concise | detailed | casual | formal
	// OutputFormat 输出格式偏好。
	OutputFormat string `json:"output_format"` // markdown | plain | code-first
	// Expertise 领域专业程度（影响解释深度和术语使用）。
	Expertise string `json:"expertise"` // novice | intermediate | expert
	// InteractionSummary LLM 在会话结束时生成的用户特征摘要（≤200 字）。
	InteractionSummary string `json:"interaction_summary"`
	// UpdatedAt Unix 秒，记录最近更新时间。
	UpdatedAt int64 `json:"updated_at"`
}

const profilePreferenceKey = "user_profile"

// PersonaRefiner 从会话信号中精炼用户画像，跨会话持久化。
type PersonaRefiner struct {
	db       *sql.DB
	provider protocol.Provider // 用于 RefineAtSessionEnd（可 nil，则跳过 LLM 更新）
	mu       sync.Mutex
	profile  *UserProfile
}

// NewPersonaRefiner 创建 PersonaRefiner。provider 为 nil 时跳过会话结束 LLM 摘要更新。
func NewPersonaRefiner(db *sql.DB, provider protocol.Provider) *PersonaRefiner {
	return &PersonaRefiner{
		db:       db,
		provider: provider,
		profile:  defaultUserProfile(),
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
		return err
	}
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return json.Unmarshal([]byte(raw), pr.profile)
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
		return err
	}
	_, err = pr.db.ExecContext(ctx, `
		INSERT INTO preferences (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, profilePreferenceKey, string(raw))
	return err
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

// RefineAtSessionEnd 会话结束时用 LLM 更新 InteractionSummary（可选，provider 为 nil 时跳过）。
// msgs 为本次会话消息列表（非 system 部分）。
func (pr *PersonaRefiner) RefineAtSessionEnd(ctx context.Context, msgs []protocol.Message) error {
	if pr.provider == nil || len(msgs) == 0 {
		return nil
	}

	var sb strings.Builder
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		sb.WriteString(m.Role)
		sb.WriteString(": ")
		// 截断单条消息避免超长
		c := m.Content
		if len(c) > 200 {
			c = c[:200] + "..."
		}
		sb.WriteString(c)
		sb.WriteByte('\n')
	}

	prompt := "Based on the following conversation, describe the user's key characteristics in 1-2 sentences. Focus on communication style, expertise level, and preferences. Output ONLY the description.\n\n" + sb.String()

	resp, err := pr.provider.Infer(ctx, &protocol.InferRequest{
		Messages:        []protocol.Message{{Role: "user", Content: prompt}},
		MaxTokens:       200,
		Temperature:     0.2,
		ReasoningEffort: protocol.ReasoningEffortLow,
	})
	if err != nil {
		return nil // LLM 失败不阻断会话结束流程
	}

	pr.mu.Lock()
	pr.profile.InteractionSummary = strings.TrimSpace(resp.Content)
	pr.mu.Unlock()
	return nil
}

// Profile 返回当前用户画像的快照（只读副本）。
func (pr *PersonaRefiner) Profile() UserProfile {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	return *pr.profile
}

// ToUserPreferences 将画像转换为 protocol.UserPreference 列表，供 ImmutableCore 注入。
func (pr *PersonaRefiner) ToUserPreferences() []protocol.UserPreference {
	pr.mu.Lock()
	p := *pr.profile
	pr.mu.Unlock()

	prefs := []protocol.UserPreference{
		{Dimension: "language_pref", PreferenceText: p.LanguagePref, Confidence: 0.9},
		{Dimension: "response_style", PreferenceText: p.ResponseStyle, Confidence: 0.8},
		{Dimension: "output_format", PreferenceText: p.OutputFormat, Confidence: 0.8},
		{Dimension: "expertise", PreferenceText: p.Expertise, Confidence: 0.7},
	}
	if p.InteractionSummary != "" {
		prefs = append(prefs, protocol.UserPreference{
			Dimension:      "interaction_summary",
			PreferenceText: p.InteractionSummary,
			Confidence:     0.6,
		})
	}
	return prefs
}
