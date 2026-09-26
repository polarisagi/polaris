// Package elicitation 实现网关侧 MCP elicitation broker（spec_elicitation.md
// 2026-07-28 client/elicitation）：接收 MCPManager 转发的 elicitation/create 请求，
// 优先交给 hooks.json 引擎程序化作答，否则登记为 pending 并通知用户，等待 HTTP
// 层 Respond 或超时/取消结束。Broker 实现 internal/extension/mcp.Elicitor 接口，
// 由 cmd/polaris 启动装配时注入 MCPManager（gateway → extension 依赖方向合法，
// 见 internal/lint Test_inv_NoCrossLayerImport；反向禁止）。
package elicitation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/polarisagi/polaris/internal/action/hook"
	"github.com/polarisagi/polaris/internal/extension/mcp"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// defaultElicitTimeout 用户未在此窗口内作答时视为 cancel（spec_elicitation.md
// §Response Actions：cancel 语义含"用户未做出明确选择"，超时属此类）。
const defaultElicitTimeout = 10 * time.Minute

// HookFirer broker 依赖的 hook 引擎能力（调用方定义接口，HE-3；实现为
// *internal/action/hook.Runner）。签名与 internal/action/hook/firer.go 一致。
type HookFirer interface {
	// FireElicitation 尝试程序化作答；decided=false 表示未命中，调用方需继续走
	// 用户交互路径。
	FireElicitation(ctx context.Context, serverName, mode, message, url, elicitationID string, schema json.RawMessage) (hook.ElicitDecision, bool)
	// FireElicitationResult 用户作答后回传服务器前的最后一道观察/覆盖关卡。
	FireElicitationResult(ctx context.Context, serverName, mode, elicitationID, action string, content map[string]any) (string, map[string]any)
	// FireNotification 通知类事件，无返回值。
	FireNotification(ctx context.Context, message, notificationType string)
}

// pendingElicitation 一次挂起中的 elicitation。仅驻留内存（HE-6 例外）：
// 它绑定的是一次正在执行、尚未返回的 MCP 工具调用 goroutine（Elicit 阻塞在
// select 上），进程重启时该 goroutine 与其所属的工具调用本身已经中断、无法续跑，
// 落盘也无法恢复语义，因此不落库不是偷懒而是没有可持久化的对应状态。
type pendingElicitation struct {
	id         string
	sessionID  string
	serverName string
	mode       string // "form" / "url"，MCPManager 层已保证非空
	message    string
	schema     json.RawMessage // form 模式的 requestedSchema，原样保留供展示与校验
	url        string          // url 模式的目标 URL，只展示不预取
	createdAt  time.Time
	respCh     chan mcp.ElicitResult // 容量 1；Respond 校验通过后写入一次
}

// PendingView Pending() 对外暴露的只读快照（HTTP JSON 序列化用）。
type PendingView struct {
	ID              string          `json:"id"`
	SessionID       string          `json:"session_id"`
	ServerName      string          `json:"server_name"`
	Mode            string          `json:"mode"`
	Message         string          `json:"message"`
	RequestedSchema json.RawMessage `json:"requested_schema,omitempty"`
	URL             string          `json:"url,omitempty"`
	CreatedAt       string          `json:"created_at"` // RFC3339
}

// Broker 网关侧 elicitation 中枢：实现 mcp.Elicitor，供 MCPManager.SetElicitor 注入；
// 同时向 HTTP 层暴露 Pending/Respond 供 WebUI 渲染对话框与提交作答。
type Broker struct {
	hooks   HookFirer // 可为 nil：表示未接入 hooks.json 引擎，所有请求都走用户交互路径
	timeout time.Duration

	mu      sync.Mutex
	pending map[string]*pendingElicitation
}

// NewBroker 构造 Broker。hooks 可为 nil；timeout<=0 时使用 defaultElicitTimeout。
func NewBroker(hooks HookFirer, timeout time.Duration) *Broker {
	if timeout <= 0 {
		timeout = defaultElicitTimeout
	}
	return &Broker{hooks: hooks, timeout: timeout, pending: make(map[string]*pendingElicitation)}
}

// Elicit 实现 mcp.Elicitor：先尝试 hook 程序化作答，未命中则转人工交互。
func (b *Broker) Elicit(ctx context.Context, req mcp.ElicitRequest) (mcp.ElicitResult, error) {
	if b.hooks != nil {
		if result, decided := b.elicitViaHook(ctx, req); decided {
			return result, nil
		}
	}
	return b.elicitViaUser(ctx, req)
}

// elicitViaHook 步骤 1：hook 已作答（decided=true）时，form 模式的 accept 内容
// 必须通过 requestedSchema 校验，不通过则降级为 decline——绝不把未经校验的内容
// 透传给 MCP 服务器，也绝不因此弹出用户对话框（规范原文允许 hook 完全接管本次
// 交互）。
func (b *Broker) elicitViaHook(ctx context.Context, req mcp.ElicitRequest) (mcp.ElicitResult, bool) {
	decision, decided := b.hooks.FireElicitation(ctx, req.ServerName, req.Mode, req.Message, req.URL, req.ElicitationID, req.RequestedSchema)
	if !decided {
		return mcp.ElicitResult{}, false
	}
	result := mcp.ElicitResult{Action: decision.Action, Content: decision.Content}
	if result.Action == "accept" && req.Mode == "form" {
		if err := validateFormContent(req.RequestedSchema, result.Content); err != nil {
			return mcp.ElicitResult{Action: "decline"}, true
		}
	}
	return result, true
}

// elicitViaUser 步骤 2~3：登记 pending、通知用户、等待终结（Respond / 超时 / ctx
// 取消三选一），无论哪种结束都要移除 pending（defer 保证）。
func (b *Broker) elicitViaUser(ctx context.Context, req mcp.ElicitRequest) (mcp.ElicitResult, error) {
	pe, err := b.registerPending(req)
	if err != nil {
		return mcp.ElicitResult{}, err
	}
	defer b.removePending(pe.id)

	if b.hooks != nil {
		b.hooks.FireNotification(ctx,
			fmt.Sprintf("MCP server %s requests input: %s", req.ServerName, req.Message),
			"elicitation_dialog")
	}

	// 用可停止的计时器：作答通常远早于 10 分钟超时，time.After 的计时器会一直存活到触发。
	timer := time.NewTimer(b.timeout)
	defer timer.Stop()
	select {
	case res := <-pe.respCh:
		action, content := res.Action, res.Content
		if b.hooks != nil {
			// 步骤 3：仅"用户作答"这一分支调用 FireElicitationResult——超时/ctx
			// 取消是 broker 合成的 cancel，不是用户的显式动作，不经过该 hook。
			action, content = b.hooks.FireElicitationResult(ctx, req.ServerName, req.Mode, req.ElicitationID, action, content)
		}
		return mcp.ElicitResult{Action: action, Content: content}, nil
	case <-timer.C:
		return mcp.ElicitResult{Action: "cancel"}, nil
	case <-ctx.Done():
		return mcp.ElicitResult{Action: "cancel"}, nil
	}
}

// registerPending 生成随机 id 并登记 pending（crypto/rand，防止 id 被猜测后抢答）。
func (b *Broker) registerPending(req mcp.ElicitRequest) (*pendingElicitation, error) {
	id, err := newPendingID()
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "elicitation: generate pending id", err)
	}
	pe := &pendingElicitation{
		id: id, sessionID: req.SessionID, serverName: req.ServerName, mode: req.Mode,
		message: req.Message, schema: req.RequestedSchema, url: req.URL,
		createdAt: time.Now(), respCh: make(chan mcp.ElicitResult, 1),
	}
	b.mu.Lock()
	b.pending[id] = pe
	b.mu.Unlock()
	return pe, nil
}

func (b *Broker) removePending(id string) {
	b.mu.Lock()
	delete(b.pending, id)
	b.mu.Unlock()
}

// Pending 返回当前挂起的 elicitation 快照（sessionID 为空返回全部），按创建时间
// 升序排序。message/schema/url 均来自不可信的 MCP 服务器，本方法只做只读展示，
// 不对其内容做任何解释或请求。
func (b *Broker) Pending(sessionID string) []PendingView {
	b.mu.Lock()
	defer b.mu.Unlock()

	entries := make([]*pendingElicitation, 0, len(b.pending))
	for _, pe := range b.pending {
		if sessionID != "" && pe.sessionID != sessionID {
			continue
		}
		entries = append(entries, pe)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].createdAt.Before(entries[j].createdAt) })

	views := make([]PendingView, 0, len(entries))
	for _, pe := range entries {
		views = append(views, PendingView{
			ID: pe.id, SessionID: pe.sessionID, ServerName: pe.serverName, Mode: pe.mode,
			Message: pe.message, RequestedSchema: pe.schema, URL: pe.url,
			CreatedAt: pe.createdAt.UTC().Format(time.RFC3339),
		})
	}
	return views
}

// Respond 用户对某个 pending elicitation 的作答。校验失败时 pending 保持挂起
// （允许前端修正后重试），只有校验通过、真正投递给等待方后才摘除。
func (b *Broker) Respond(id string, res mcp.ElicitResult) error {
	if !isValidResponseAction(res.Action) {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("elicitation: invalid action %q", res.Action))
	}

	b.mu.Lock()
	pe, ok := b.pending[id]
	b.mu.Unlock()
	if !ok {
		return apperr.New(apperr.CodeNotFound, fmt.Sprintf("elicitation: pending %q not found", id))
	}

	content, err := validateRespondContent(pe, res.Action, res.Content)
	if err != nil {
		return err
	}

	// 二次查表 + 摘除须在同一临界区完成：避免两个并发 Respond 都通过校验后
	// 争用同一个容量 1 的 respCh（HE-2 可验证执行——一次性凭证语义，不能靠时序侥幸）。
	b.mu.Lock()
	pe, ok = b.pending[id]
	if ok {
		delete(b.pending, id)
	}
	b.mu.Unlock()
	if !ok {
		return apperr.New(apperr.CodeNotFound, fmt.Sprintf("elicitation: pending %q not found", id))
	}

	select {
	case pe.respCh <- mcp.ElicitResult{Action: res.Action, Content: content}:
	default:
		// respCh 容量 1 且此刻已从 pending map 摘除、不可能被第二次写入；
		// default 分支纯防御性，避免任何理论竞态下阻塞 HTTP 请求协程。
	}
	return nil
}

func isValidResponseAction(a string) bool {
	switch a {
	case "accept", "decline", "cancel":
		return true
	default:
		return false
	}
}

// validateRespondContent 依据 pending 记录的 mode/schema 校验用户作答内容
// （spec_elicitation.md §Response Actions + §Requested Schema）：
// decline/cancel 一律忽略 content；url 模式 accept 禁止携带 content（规范：
// URL 模式不回传数据）；form 模式 accept 必须匹配 requestedSchema。
func validateRespondContent(pe *pendingElicitation, action string, content map[string]any) (map[string]any, error) {
	if action != "accept" {
		return nil, nil
	}
	if pe.mode == "url" {
		if len(content) > 0 {
			return nil, apperr.New(apperr.CodeInvalidInput, "elicitation: url 模式 accept 不得携带 content")
		}
		return nil, nil
	}
	if err := validateFormContent(pe.schema, content); err != nil {
		return nil, err
	}
	return content, nil
}

// newPendingID 生成 32 位十六进制随机 id（128 bit 熵，防猜测抢答）。
func newPendingID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "elicitation: crypto/rand unavailable", err)
	}
	return hex.EncodeToString(buf), nil
}
