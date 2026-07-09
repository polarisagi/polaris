package cronadmin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"

	"github.com/polarisagi/polaris/configs"
	"github.com/polarisagi/polaris/pkg/types"

	"gopkg.in/yaml.v3"
)

// ─── 数据模型 ─────────────────────────────────────────────────────────────────

type automation struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Prompt          string `json:"prompt"`
	TriggerType     string `json:"trigger_type"`
	CronSchedule    string `json:"cron_schedule"`
	ChannelID       string `json:"channel_id"`
	WorkingDir      string `json:"working_dir"`
	EnvType         string `json:"env_type"`
	ReasoningEffort string `json:"reasoning_effort"`
	ResultAction    string `json:"result_action"`
	SandboxLevel    int    `json:"sandbox_level"`
	CedarRulesJSON  string `json:"cedar_rules_json"`
	Enabled         bool   `json:"enabled"`
	RequiresHITL    bool   `json:"requires_hitl"`
	RiskLevel       int    `json:"risk_level"`
	LastRunAt       string `json:"last_run_at"`
	NextRunAt       string `json:"next_run_at"`
	RunCount        int    `json:"run_count"`
	LastRunStatus   string `json:"last_run_status"`
	LastRunError    string `json:"last_run_error"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	EventFilter     string `json:"event_filter"`
}

type automationRun struct {
	ID             string `json:"id"`
	AutomationID   string `json:"automation_id"`
	Trigger        string `json:"trigger"`
	Status         string `json:"status"`
	SessionID      string `json:"session_id"`
	StartedAt      string `json:"started_at"`
	FinishedAt     string `json:"finished_at"`
	ErrorMsg       string `json:"error_msg"`
	PromptSnapshot string `json:"prompt_snapshot"`
}

func newAutoID() string {
	b := make([]byte, 8)
	rand.Read(b) //nolint:errcheck
	return "auto_" + hex.EncodeToString(b)
}

func newRunID() string {
	b := make([]byte, 8)
	rand.Read(b) //nolint:errcheck
	return "run_" + hex.EncodeToString(b)
}

// ─── GET /v1/automations ──────────────────────────────────────────────────────

func matchEventFilter(filterJSON, topic, typ, payload string) bool {
	var f map[string]interface{}
	if err := json.Unmarshal([]byte(filterJSON), &f); err != nil {
		return false
	}
	if wantTopic, ok := f["topic"].(string); ok && wantTopic != "" && wantTopic != topic {
		return false
	}
	if wantType, ok := f["type"].(string); ok && wantType != "" && wantType != typ {
		return false
	}
	// payload 子集匹配：event_filter 中的 payload 对象所有 key-value 必须在实际 payload 中存在
	if wantPayload, ok := f["payload"].(map[string]interface{}); ok && len(wantPayload) > 0 {
		var actualPayload map[string]interface{}
		if err := json.Unmarshal([]byte(payload), &actualPayload); err != nil {
			// payload 非 JSON 对象时，若 filter 有 payload 条件则不匹配
			return false
		}
		for k, wantVal := range wantPayload {
			actualVal, exists := actualPayload[k]
			if !exists {
				return false
			}
			// 字符串类型精确比较；其余类型转字符串后比较（避免数字类型不一致）
			if fmt.Sprintf("%v", actualVal) != fmt.Sprintf("%v", wantVal) {
				return false
			}
		}
	}
	return true
}

type cronCtxKey string

const (
	ctxKeySandboxLevel cronCtxKey = "sandbox_level"
	ctxKeyCedarRules   cronCtxKey = "cedar_rules_json"
)

// finalizeWorktreeChanges 提交 worktree 改动，经 HITL 审批后 push 并尝试创建 PR。
// 2026-07-04 审计修复（Task 19）：此前无条件 commit+push，LLM 生成的代码改动零人工审批
// 即可推送到 origin。现固定要求：commit（带 --author 署名）→ 若配置了 HITLGateway，
// 展示 diff 摘要请求人工批准 → 批准后才 push → push 成功后尝试创建 PR（非致命）。
// 未获批准时，改动保留在本地分支的 commit 中（不 push），由人工后续处理。
func ParseReasoningEffort(e string) types.ReasoningEffort {
	switch e {
	case "low":
		return types.ReasoningEffortLow
	case "medium":
		return types.ReasoningEffortMedium
	case "high":
		return types.ReasoningEffortHigh
	case "ultra":
		return types.ReasoningEffortHigh // ultra map to high
	default:
		return types.ReasoningEffortMedium
	}
}

// ─── Cron 表达式解析（简化版，支持标准5字段格式 + @daily/@weekly 别名）────────

// CircuitBreakThreshold 连续失败达到该阈值 → circuit_open=1，cronTick 跳过该任务，
// 直至下次成功执行自愈清零（Gap-C 电路断路器）。
const CircuitBreakThreshold = 5

// CalcNextRun 基于当前时间计算下次触发时间（RFC3339）。
func CalcNextRun(expr, fromRFC3339 string) string { //nolint:gocyclo
	from, err := time.Parse(time.RFC3339, fromRFC3339)
	if err != nil {
		from = time.Now().UTC()
	}

	// 语义别名展开
	switch strings.TrimSpace(expr) {
	case "@hourly":
		expr = "0 * * * *"
	case "@daily", "@midnight":
		expr = "0 0 * * *"
	case "@weekly":
		expr = "0 0 * * 0"
	case "@monthly":
		expr = "0 0 1 * *"
	}

	// 去掉秒字段（6字段 → 5字段）
	parts := strings.Fields(expr)
	if len(parts) == 6 {
		parts = parts[1:]
	}
	if len(parts) != 5 {
		return ""
	}

	minuteMatch := false
	hourMatch := false
	domMatch := false
	monthMatch := false
	dowMatch := false

	// parse
	minStep, minFixed := parseCronField(parts[0])
	hourStep, hourFixed := parseCronField(parts[1])
	domStep, domFixed := parseCronField(parts[2])
	monthStep, monthFixed := parseCronField(parts[3])
	dowStep, dowFixed := parseCronField(parts[4])

	t := from.Add(time.Minute).Truncate(time.Minute)
	// 从 from+1min 开始向前推，找下一个匹配时刻（最多搜索 1 年）
	for range 525600 { // 最多 365 天 × 1440 分钟
		minuteMatch = (minFixed == -1 && t.Minute()%minStep == 0) || (minFixed != -1 && t.Minute() == minFixed)
		hourMatch = (hourFixed == -1 && t.Hour()%hourStep == 0) || (hourFixed != -1 && t.Hour() == hourFixed)
		domMatch = (domFixed == -1 && t.Day()%domStep == 0) || (domFixed != -1 && t.Day() == domFixed)
		monthMatch = (monthFixed == -1 && int(t.Month())%monthStep == 0) || (monthFixed != -1 && int(t.Month()) == monthFixed)
		dowMatch = (dowFixed == -1 && int(t.Weekday())%dowStep == 0) || (dowFixed != -1 && int(t.Weekday()) == dowFixed)

		if minuteMatch && hourMatch && domMatch && monthMatch && dowMatch {
			return t.UTC().Format(time.RFC3339)
		}
		t = t.Add(time.Minute)
	}
	return ""
}

// parseCronField 解析字段，返回 step 和 fixed (-1 为通配)。
// 对于 "*" 返回 1, -1。对于 "*/n" 返回 n, -1。对于 "n" 返回 1, n。
func parseCronField(part string) (int, int) {
	if part == "*" {
		return 1, -1
	}
	if strings.HasPrefix(part, "*/") {
		step, err := strconv.Atoi(part[2:])
		if err == nil && step > 0 {
			return step, -1
		}
		return 1, -1 // fallback
	}
	if fixed, err := strconv.Atoi(part); err == nil {
		return 1, fixed
	}
	return 1, -1 // fallback
}

// ─── 自动化模板市场 ───────────────────────────────────────────────────────────

// automationTemplate 对应 configs/automations/templates/*.yaml 或远程 index.json 中的单条记录。
type automationTemplate struct {
	Icon            string   `yaml:"icon"             json:"icon"`
	Name            string   `yaml:"name"             json:"name"`
	Description     string   `yaml:"description"      json:"description"`
	Prompt          string   `yaml:"prompt"           json:"prompt"`
	TriggerType     string   `yaml:"trigger_type"     json:"trigger_type"`
	CronSchedule    string   `yaml:"cron_schedule"    json:"cron_schedule"`
	ReasoningEffort string   `yaml:"reasoning_effort" json:"reasoning_effort"`
	Tags            []string `yaml:"tags"             json:"tags,omitempty"`
	Source          string   `yaml:"source"           json:"source,omitempty"`
	Author          string   `yaml:"author"           json:"author,omitempty"`
}

// automationSource 对应 configs/automation_sources.yaml 中的单条来源配置。
type automationSource struct {
	ID          string `yaml:"id"`
	Name        string `yaml:"name"`
	Type        string `yaml:"type"` // local | remote
	Path        string `yaml:"path"` // type=local 时有效
	URL         string `yaml:"url"`  // type=remote 时有效
	Description string `yaml:"description"`
	Enabled     bool   `yaml:"enabled"`
	TrustTier   int    `yaml:"trust_tier"`
}

// remoteIndex 是远程 index.json 的顶层结构。
type remoteIndex struct {
	Templates []automationTemplate `json:"templates"`
}

// templateCache 存放远程拉取结果，避免每次请求都走网络。
type templateCache struct {
	templates []automationTemplate
	fetchedAt time.Time
}

const templateCacheTTL = time.Hour

// loadEmbeddedTemplates 从 embed.FS 读取内置模板目录（二进制完全自包含，不依赖工作目录）。
func loadEmbeddedTemplates(dir string) []automationTemplate {
	entries, err := configs.FS.ReadDir(dir)
	if err != nil {
		return nil
	}
	var all []automationTemplate
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, err := configs.FS.ReadFile(dir + "/" + e.Name())
		if err != nil {
			slog.Warn("automation-templates: embedded read failed", "file", e.Name(), "err", err)
			continue
		}
		var tpls []automationTemplate
		if err := yaml.Unmarshal(b, &tpls); err != nil {
			slog.Warn("automation-templates: embedded parse failed", "file", e.Name(), "err", err)
			continue
		}
		all = append(all, tpls...)
	}
	return all
}

// loadLocalTemplates 扫描用户配置的本地目录（automation_sources.yaml type=local 路径）。
// 仅用于 Operator 自定义模板；内置模板统一走 loadEmbeddedTemplates。
func loadLocalTemplates(dir string) []automationTemplate {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var all []automationTemplate
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			slog.Warn("automation-templates: read failed", "file", e.Name(), "err", err)
			continue
		}
		var tpls []automationTemplate
		if err := yaml.Unmarshal(b, &tpls); err != nil {
			slog.Warn("automation-templates: parse failed", "file", e.Name(), "err", err)
			continue
		}
		all = append(all, tpls...)
	}
	return all
}

// fetchRemoteTemplates 拉取远程 index.json，命中缓存则直接返回。
func (ca *CronAdmin) fetchRemoteTemplates(src automationSource) []automationTemplate {
	if val, ok := ca.TemplateCacheMap.Load(src.ID); ok {
		if c, isType := val.(*templateCache); isType && time.Since(c.fetchedAt) < templateCacheTTL {
			return c.templates
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.URL, nil)
	if err != nil {
		slog.Warn("automation-templates: bad remote url", "id", src.ID, "err", err)
		return nil
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "polaris/1.0")

	resp, err := ca.HTTPClient.Do(req)
	if err != nil {
		slog.Warn("automation-templates: fetch failed", "id", src.ID, "err", err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Warn("automation-templates: remote returned non-200", "id", src.ID, "status", resp.StatusCode, "err", apperr.New(apperr.CodeInternal, "log event"))
		return nil
	}

	var idx remoteIndex
	if err := json.NewDecoder(resp.Body).Decode(&idx); err != nil {
		slog.Warn("automation-templates: decode failed", "id", src.ID, "err", err)
		return nil
	}

	// 注入来源标识（覆盖远程可能缺失的 source 字段）
	for i := range idx.Templates {
		if idx.Templates[i].Source == "" {
			idx.Templates[i].Source = src.ID
		}
	}

	ca.TemplateCacheMap.Store(src.ID, &templateCache{templates: idx.Templates, fetchedAt: time.Now()})
	slog.Info("automation-templates: remote fetched", "id", src.ID, "count", len(idx.Templates))
	return idx.Templates
}

// loadSources 从 embed.FS 读取 extensions/automation_sources.yaml（二进制自包含，不依赖工作目录）。
func loadSources() []automationSource {
	b, err := configs.FS.ReadFile("extensions/automation_sources.yaml")
	if err != nil {
		return nil
	}
	var srcs []automationSource
	if err := yaml.Unmarshal(b, &srcs); err != nil {
		slog.Warn("automation-sources: parse failed", "err", err)
		return nil
	}
	return srcs
}

// GET /v1/automation-templates
// 合并所有已启用来源（local YAML + 远程 index）返回模板列表。
// 查询参数：?source=<id> 可过滤单一来源；?tag=<tag> 过滤标签。
func NewSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
