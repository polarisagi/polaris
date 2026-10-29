package main

import (
	"context"
	"log/slog"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/pb"
	"github.com/polarisagi/polaris/pkg/types"
	"github.com/polarisagi/polaris/pkg/util"
)

// streamInterruptEventLogger 把 llm.StreamInterruptRecorder 桥接到 M2 EventLog。
//
// inv_M1_04（`docs/arch/M01-Inference-Runtime.md §0-ter`）：「流中断时 EventLog
// 追加 streaming_interrupted: true 字段，禁止静默丢弃」。2026-08-10 之前该不变量
// 零实现——internal/llm 全包检索不到 streaming_interrupted，流中断只留下一条
// slog.Warn 和一次 trace.RecordLLMCall，两者都不进 events 表。
//
// 装配放在 cmd/polaris 而非 internal/llm：EventLogger 属 L0 存储层，
// internal/llm 直接依赖它会把"推理运行时"绑到具体存储实现上（HE-3 接口在调用方
// 定义，装配在装配层完成）。
type streamInterruptEventLogger struct {
	eventLog protocol.EventLogger
}

// RecordStreamInterrupted 落一条 topic=llm.stream / type=streaming_interrupted 的事件。
//
// payload 用 LLMCallPayload 复用既有 schema：FinishReason 写具体中断原因
// （budget_guard:… / context_cancelled:…），避免为一个布尔标记新增 proto 字段
// 并触发全量 pb 重生成。事件 type 本身即承载 inv_M1_04 要求的
// "streaming_interrupted: true" 语义——按 type 过滤 events 表即可取全部中断记录。
func (l streamInterruptEventLogger) RecordStreamInterrupted(ctx context.Context, provider, reason string) {
	if l.eventLog == nil {
		return
	}
	now := time.Now().UnixMicro()
	evID := util.GenerateHumanReadableID("evt", "stream interrupted")
	ev := &pb.Event{
		Id:    evID,
		Topic: "llm.stream",
		Actor: "system",
		Type:  "streaming_interrupted",
		// 每次中断都是独立事实，不可去重：entityID 用事件自身 ID（唯一），
		// version 恒 0。（见 §幂等键统一格式——不可去重的事件靠唯一后缀而非塞 version。）
		IdempotencyKey: string(types.BuildIdempotencyKey("llm", "stream_interrupt", evID, "record", 0)),
		OccurredAt:     now,
		CreatedAt:      now,
		Payload:        marshalStreamInterruptPayload(provider, reason),
	}
	// 审计写失败不回传给推理路径：流已经断了，再让调用方为"记录失败"多失败一次
	// 没有意义。但要留 Error 级痕迹——inv_M1_04 的"禁止静默丢弃"同样适用于
	// 记录动作本身失败的情形。
	if err := l.eventLog.AppendEvent(ctx, ev); err != nil {
		slog.Error("llm: streaming_interrupted 事件落盘失败，本次中断不可追溯（inv_M1_04）",
			"provider", provider, "reason", reason, "err", err)
	}
}

func marshalStreamInterruptPayload(provider, reason string) []byte {
	payload, err := proto.Marshal(&pb.LLMCallPayload{
		Provider:     provider,
		FinishReason: reason,
	})
	if err != nil {
		return nil
	}
	return payload
}
