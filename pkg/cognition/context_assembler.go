package cognition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	perrors "github.com/polarisagi/polaris/internal/errors"
	"github.com/polarisagi/polaris/internal/protocol"
)

// ContextAssembler — LLM 调用前上下文组装器。
// 架构文档: docs/arch/05-Memory-System-深度选型.md §2.1, §11
// 组装顺序: ZoneImmutable → ZoneMutableSkill → ZoneTaintedData（不可变区永远在最前）

// ContextZone 污点分级（3 级）。
type ContextZone int

const (
	ZoneImmutable    ContextZone = iota // 用户身份/安全约束，永不裁剪
	ZoneMutableSkill                    // SKILL.md 模板，M9 可优化
	ZoneTaintedData                     // 外部数据，Taint Tracking
)

// BuildContext 组装 LLM prompt 上下文（每次 LLM 调用前）。
// 5 个 Layout Zone 映射到 3 个 ContextZone:
//
//	Zone 1 Immutable Core (10%)   → ZoneImmutable
//	Zone 2 Tool Definitions (15%) → ZoneImmutable
//	Zone 3 Retrieved Knowledge (35%) → ZoneTaintedData, [Taint-Prop] 强制传播
//	Zone 4 Recent History (25%)   → ZoneTaintedData, [Taint-Prop] 强制传播
//	Zone 5 Buffer (15%)           → output only
//
// 安全约束: Zone 3/4 内容若 [TaintLevel] >= [Taint-Medium] → 禁止进入 instruction slot。
func BuildContext(wm *WorkingMemory, maxTokens int) *ContextLayout {
	layout := &ContextLayout{
		zones: make([]ContextChunk, 0, 5),
	}

	budget := float64(maxTokens)

	// Zone 1 — Immutable Core (10%)
	immutableTokens := int(budget * 0.10)
	layout.zones = append(layout.zones, ContextChunk{
		Zone:      ZoneImmutable,
		Content:   "<immutable core>",
		MaxTokens: immutableTokens,
	})

	// Zone 2 — Tool Definitions (15%)
	toolTokens := int(budget * 0.15)
	layout.zones = append(layout.zones, ContextChunk{
		Zone:      ZoneImmutable,
		Content:   "", // tools injected by M4 from ToolRegistry
		MaxTokens: toolTokens,
	})

	// Zone 3 — Retrieved Knowledge (35%)
	knowledgeTokens := int(budget * 0.35)
	layout.zones = append(layout.zones, ContextChunk{
		Zone:      ZoneTaintedData,
		Content:   "", // filled by HybridRetriever
		MaxTokens: knowledgeTokens,
		Tainted:   true,
	})

	// Zone 4 — Recent History (25%)
	historyTokens := int(budget * 0.25)
	layout.zones = append(layout.zones, ContextChunk{
		Zone:      ZoneTaintedData,
		Content:   "", // recent 32 session events
		MaxTokens: historyTokens,
		Tainted:   true,
	})

	// Zone 5 — Buffer (15%), output only
	bufferTokens := int(budget * 0.15)
	layout.zones = append(layout.zones, ContextChunk{
		Zone:      ZoneTaintedData,
		MaxTokens: bufferTokens,
		Output:    true,
	})

	return layout
}

// ContextLayout 上下文布局。
type ContextLayout struct {
	zones       []ContextChunk
	Epoch       int64              // 上下文版本号（由 EpochTracker 维护，每次内容变化递增）
	Fingerprint ContextFingerprint // 当前 layout 的 SHA256 摘要
}

// ContextChunk 上下文字段。
type ContextChunk struct {
	Zone      ContextZone
	Content   string
	MaxTokens int
	Tainted   bool
	Output    bool
}

// GroundingResult 包含评估结果。
type GroundingResult struct {
	Sufficient bool
	Warning    string
}

// AssembleContext 组装 Agent LLM 调用的完整上下文。
// 顺序: ZoneImmutable → ZoneMutableSkill → ZoneTaintedData
// 安全约束: ZoneImmutable 内容 TaintLevel > TaintLow → panic 拒绝
func AssembleContext(
	ctx context.Context,
	immutable, mutableSkill, taintedData string,
	taintLevel protocol.TaintLevel,
	wm *WorldModel,
	task string,
) (string, *GroundingResult) {
	var b strings.Builder
	b.WriteString(immutable)
	b.WriteString(mutableSkill)

	if taintLevel >= protocol.TaintMedium {
		hash := sha256.Sum256([]byte(taintedData))
		hexStr := hex.EncodeToString(hash[:])[:8]
		b.WriteString(fmt.Sprintf("\n=== UNTRUSTED_DATA_%s ===\n", hexStr))
		b.WriteString(taintedData)
		b.WriteString("\n=== END_UNTRUSTED_DATA ===\n")
	} else {
		b.WriteString(taintedData)
	}

	fullContext := b.String()
	res := &GroundingResult{Sufficient: true}

	if wm != nil && task != "" {
		suff, warning := wm.AssessGrounding(ctx, task, fullContext)
		res.Sufficient = suff
		res.Warning = warning
		if !suff {
			// 将缺失提示作为系统级别的 meta 注入到上下文末尾
			fullContext += fmt.Sprintf("\n[System Warning: Knowledge gap detected. Consider further retrieval before action. Gap: %s]\n", warning)
		}
	}

	return fullContext, res
}

// ValidateZoneWrite 验证向指定 Zone 写入的 TaintLevel 合法性。
// ZoneImmutable: 仅接受 TaintLow 及以下。
// ZoneMutableSkill: 需 Ed25519 签名验证（M9 签发）。
// 其他: 接受任意 TaintLevel。
func ValidateZoneWrite(zone ContextZone, taintLevel protocol.TaintLevel) error {
	switch zone {
	case ZoneImmutable:
		if taintLevel > protocol.TaintLow {
			return perrors.New(perrors.CodeInternal, fmt.Sprintf("context_assembler: taint level %s rejected from ZoneImmutable (max TaintLow)", taintLevel.String()))
		}
	case ZoneMutableSkill:
		if taintLevel >= protocol.TaintMedium {
			return perrors.New(perrors.CodeInternal, fmt.Sprintf("context_assembler: unsanitized taint level %s rejected from ZoneMutableSkill", taintLevel.String()))
		}
	}
	return nil
}
