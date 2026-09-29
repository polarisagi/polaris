package main

import (
	"context"
	"log/slog"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/store"
	"github.com/polarisagi/polaris/pkg/offpeak"
)

// newOffpeakGate 由 m1_router.offpeak.windows 构造错峰 Gate（ADR-0105 决策七）。
// 窗口为空返回 nil（不错峰，所有接入点零开销）。窗口格式已在 config 校验期拒绝非法值，
// 这里再次解析失败只可能是绕过校验的调用方——按"不错峰"降级并告警，绝不因错峰配置让后台任务永久停摆。
func newOffpeakGate(t config.M1RouterThresholds) *offpeak.Gate {
	w, err := offpeak.Parse(t.OffpeakWindows)
	if err != nil {
		slog.Warn("polaris: offpeak.windows 无效，已降级为不错峰", "err", err)
		return nil
	}
	gate := offpeak.NewGate(w)
	if gate != nil {
		slog.Info("polaris: 错峰调度已启用", "windows_utc", t.OffpeakWindows)
	}
	return gate
}

// deferOffPeak 包装 outbox handler：窗口外不执行，返回 OffPeakDeferral 让 outbox 把该条推迟到
// 下一个窗口起点（不计失败、不进死信，见 internal/store/outbox_worker.go markNonFailure）。
// gate 为 nil 时原样返回 h，零开销。
//
// 只应包装"可延迟的批处理投影"（建图/摘要/技能合成）；交互路径与回合终态的记忆巩固
// （consolidate_summary 等）不得经此包装。
func deferOffPeak(gate *offpeak.Gate, h store.OutboxHandler) store.OutboxHandler {
	if gate == nil {
		return h
	}
	return func(ctx context.Context, rec *store.OutboxRecord) error {
		if until, wait := gate.Until(); wait {
			return &protocol.OffPeakDeferral{Until: until}
		}
		return h(ctx, rec)
	}
}
