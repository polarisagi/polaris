// Package notify 实现 GD-13-001 异步长程任务离线通知推送的最小方案：
// 消费 Outbox TopicNotification 条目，投递到用户配置的 Webhook URL。
//
// 范围边界（明确不做）：
//   - 不做通知偏好的前端配置界面，复用已有的 preferences 表（016_preferences.sql）
//     KV 存储承载 notification_webhook_url / notification_enabled 两个键，不新增表/字段。
//   - 不实现 IM/邮件渠道（后续迭代复用 internal/channel），本轮只做 Webhook（HTTP POST）。
//   - 不做"用户是否在线"判断，写入时机由生产者（internal/automation.SQLiteScheduler）
//     按 Task.Pool != "intent_handler"（非用户交互任务）过滤决定，本包只负责投递。
//
// 失败重试复用 internal/store.OutboxWorker 已有的指数退避机制：Handle 返回非 nil
// error 时，OutboxWorker.processAndMark 自动标记 failed 并按 2^attempt×5s 退避重试，
// 达到 maxRetries 后转 dead——本包不自建重试逻辑（HE-3 可组合原语）。
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// PreferenceReader 抽象偏好读取（接口在调用方定义，HE-3）。
// 由 internal/store/repo.SQLiteSystemRepository 满足。
type PreferenceReader interface {
	GetPreference(ctx context.Context, key string) (string, error)
}

const (
	// PrefWebhookURL 通知 Webhook 目标地址；为空表示未配置，跳过投递。
	PrefWebhookURL = "notification_webhook_url"
	// PrefEnabled 通知开关；显式设为 "false" 时即使配置了 URL 也不投递
	// （用户"从不通知"选项）。未设置或非 "false" 视为启用。
	PrefEnabled = "notification_enabled"

	httpTimeout = 10 * time.Second
)

// NotificationEvent 是写入 Outbox TopicNotification 的负载结构，
// 由 internal/automation.SQLiteScheduler 在任务终态时生成。
type NotificationEvent struct {
	TaskID    string `json:"task_id"`
	TaskType  string `json:"task_type"`
	Pool      string `json:"pool"` // background / cron / eval / ingest（不含 intent_handler）
	Success   bool   `json:"success"`
	Error     string `json:"error,omitempty"`
	Timestamp int64  `json:"timestamp"`
}

// Dispatcher 是 Outbox "notification" TargetEngine 的消费端处理器。
type Dispatcher struct {
	prefs      PreferenceReader
	httpClient *http.Client
}

// NewDispatcher 创建通知投递处理器。
func NewDispatcher(prefs PreferenceReader) *Dispatcher {
	return &Dispatcher{
		prefs:      prefs,
		httpClient: &http.Client{Timeout: httpTimeout},
	}
}

// Handle 实现 store.OutboxHandler 签名，供 OutboxWorker.RegisterHandler 注册。
func (d *Dispatcher) Handle(ctx context.Context, record *store.OutboxRecord) error {
	var ev NotificationEvent
	if err := json.Unmarshal(record.Payload, &ev); err != nil {
		// 畸形 payload：跳过而非无限重试（与 boot_agent.go TopicAgentInterrupt 先例一致）。
		return nil //nolint:nilerr // malformed payload 跳过，避免 OutboxWorker 无限重试
	}

	if d.prefs == nil {
		return nil
	}
	enabled, err := d.prefs.GetPreference(ctx, PrefEnabled)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "notify: read notification_enabled failed", err)
	}
	if enabled == "false" {
		return nil
	}
	webhookURL, err := d.prefs.GetPreference(ctx, PrefWebhookURL)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "notify: read notification_webhook_url failed", err)
	}
	if webhookURL == "" {
		// 未配置 Webhook：视为用户未开启通知，正常消费（非错误，不重试）。
		return nil
	}

	body, err := json.Marshal(ev)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "notify: marshal notification event failed", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "notify: build webhook request failed", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.httpClient.Do(req)
	if err != nil {
		// 网络错误：返回错误触发 OutboxWorker 退避重试。
		return apperr.Wrap(apperr.CodeNetworkUnavailable, "notify: webhook delivery failed", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return apperr.New(apperr.CodeNetworkUnavailable,
			"notify: webhook returned non-2xx status "+resp.Status)
	}
	return nil
}
