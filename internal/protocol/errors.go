package protocol

import (
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// ErrAllProvidersFailed 所有 Provider 耗尽哨兵
var ErrAllProvidersFailed = apperr.NewSentinel(apperr.CodeInternal, "inference: all providers exhausted")

// ErrBackgroundDeferred 可推迟的后台工作（WithDeferrableBackgroundWork）在资源水位线下
// 被拒绝准入。它不是失败：调用方应择机重试且不计入失败次数。
var ErrBackgroundDeferred = apperr.NewSentinel(apperr.CodeResourceExhausted, "background work deferred under resource pressure")

// ErrContextOverflow 请求超出模型上下文窗口或 payload 上限。这是请求侧故障而非 Provider
// 故障：原样重发给任何 Provider 都会再次失败，路由不重试、不 failover、不计入熔断，
// 立即交还调用方；调用方须先缩减消息再重试（agent 的溢出恢复见 agent_overflow_recovery.go）。
var ErrContextOverflow = apperr.NewSentinel(apperr.CodeInvalidInput, "inference: request exceeds model context window")

// OffPeakDeferral 可延迟后台任务落在错峰窗口之外（ADR-0105 决策七）：不是失败，
// outbox 应把该条推迟到 Until（下一个窗口起点）再执行，且不计入重试/死信。
// 与 ErrBackgroundDeferred 分开：后者是"资源水位线，30s 后再试"，这里是"按配置的时刻表，
// 到点才试"——用固定 30s 轮询去等几小时后的窗口，只会白白空转 outbox。
type OffPeakDeferral struct{ Until time.Time }

func (e *OffPeakDeferral) Error() string {
	return "background work deferred until off-peak window " + e.Until.UTC().Format(time.RFC3339)
}
