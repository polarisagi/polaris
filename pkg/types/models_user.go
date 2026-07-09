package types

type

// UserProfile 用户画像（L3 Persona）。
// 由 M5 ConsolidationPipeline Stage 3.5 每 50 条新事件自动合成，随使用演化。
UserProfile struct {
	ProfileKey         string         `json:"profile_key"`         // 默认 'default'
	StableFacts        map[string]any `json:"stable_facts"`        // 低频变化事实（角色/技能/偏好）
	RecentActivity     []string       `json:"recent_activity"`     // 近 7d 行为摘要（最多 20 条）
	BehavioralPatterns map[string]any `json:"behavioral_patterns"` // 工具频率/编码风格/沟通习惯
	SynthesisCount     int            `json:"synthesis_count"`     // 累计合成次数
	LastEventTS        int64          `json:"last_event_ts"`       // 最后消费事件的 Unix 毫秒时间戳
}

type

// ImmutableCoreView ImmutableCore 加载结果（永不裁剪的核心区快照）。
ImmutableCoreView struct {
	UserPrefs   []UserPreference
	SessionGoal string
	SafetyRules []SafetyRule
}

type

// UserPreference 用户偏好条目（ImmutableCore 的一条记录）。
UserPreference struct {
	Dimension      string
	PreferenceText string
	Confidence     float64
	ProvenanceID   string // staging_candidates full_promotion ID
}
