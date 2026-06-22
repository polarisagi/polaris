package agentctx

import "github.com/polarisagi/polaris/internal/protocol"

// MemoryWhisper 来自 MemoryAgent 的耳语线索（异步推送到主脑）。
type MemoryWhisper = protocol.MemoryWhisper

// WhisperChanCap 耳语通道容量。
const WhisperChanCap = 16
