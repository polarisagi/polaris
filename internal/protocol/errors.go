package protocol

import "github.com/polarisagi/polaris/pkg/apperr"

// ErrAllProvidersFailed 所有 Provider 耗尽哨兵
var ErrAllProvidersFailed = apperr.NewSentinel(apperr.CodeInternal, "inference: all providers exhausted")

// ErrBackgroundDeferred 可推迟的后台工作（WithDeferrableBackgroundWork）在资源水位线下
// 被拒绝准入。它不是失败：调用方应择机重试且不计入失败次数。
var ErrBackgroundDeferred = apperr.NewSentinel(apperr.CodeResourceExhausted, "background work deferred under resource pressure")
