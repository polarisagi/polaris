package regression

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// Report represents a lightweight regression detection report.
type Report struct {
	Markdown string
	Severity string // "Severe", "Warning", "Minor", "Pass"
}

// regressionWindowSize 每个对比窗口的 EventLog 事件条数（近期窗口 vs 基线窗口）。
// 选取 200：Tier-0 (2GB) 场景下单窗口查询耗时 <50ms，且足以覆盖多轮任务的行为分布。
const regressionWindowSize = 200

// LightweightRegressionDetector 基于 EventLog（001_events 表）做轻量层间回归对比。
//
// 设计取舍（2026-07-04 审计修复 Task 21+22）：
//   - events.payload 为 Protobuf BLOB，逐条反序列化对比属于重量级实现，超出"lightweight"设计目标
//     [CLAUDE.md 禁止超前抽象、臆测开发]。故本实现仅对比 events 表的结构化列（type / topic），
//     不解码 payload —— 属于真实但简化的对比，而非此前的完全硬编码假报告。
//   - 对比维度：(1) 近期窗口相对基线窗口是否出现新的事件 type（行为面变化信号）；
//     (2) 错误类事件（topic 形如 "*.error" / "*.fail*" / "safety.*"）占比是否显著上升。
type LightweightRegressionDetector struct {
	db protocol.SQLQuerier
}

// NewLightweightRegressionDetector creates a new regression detector.
func NewLightweightRegressionDetector(db protocol.SQLQuerier) *LightweightRegressionDetector {
	return &LightweightRegressionDetector{db: db}
}

// windowStats 查询 (offsetGT, offsetLTE] 区间内按 type 分组的事件计数，以及匹配错误类 topic 的事件数。
func (d *LightweightRegressionDetector) windowStats(ctx context.Context, offsetGT, offsetLTE int64) (map[string]int64, int64, int64, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT type, COUNT(*) FROM events WHERE offset > ? AND offset <= ? GROUP BY type`,
		offsetGT, offsetLTE,
	)
	if err != nil {
		return nil, 0, 0, apperr.Wrap(apperr.CodeInternal, "LightweightRegressionDetector: query type distribution failed", err)
	}
	defer rows.Close()

	typeCounts := make(map[string]int64)
	var total int64
	for rows.Next() {
		var t string
		var c int64
		if err := rows.Scan(&t, &c); err != nil {
			return nil, 0, 0, apperr.Wrap(apperr.CodeInternal, "LightweightRegressionDetector: scan type row failed", err)
		}
		typeCounts[t] = c
		total += c
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, apperr.Wrap(apperr.CodeInternal, "LightweightRegressionDetector: rows iteration failed", err)
	}

	var errCount int64
	errRow := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE offset > ? AND offset <= ? AND (topic LIKE '%.error' OR topic LIKE '%.fail%' OR topic LIKE 'safety.%')`,
		offsetGT, offsetLTE,
	)
	if err := errRow.Scan(&errCount); err != nil {
		return nil, 0, 0, apperr.Wrap(apperr.CodeInternal, "LightweightRegressionDetector: scan error count failed", err)
	}

	return typeCounts, total, errCount, nil
}

// DetectRegression 对比最近窗口与基线窗口的 EventLog 行为分布，生成轻量回归报告。
// module 目前仅用于报告标题标注（调用方语义见 internal/automation/hitl/gateway.go），
// EventLog 当前无按模块切分的结构化字段，暂不支持按 module 过滤查询。
func (d *LightweightRegressionDetector) DetectRegression(ctx context.Context, module string) (*Report, error) {
	if d.db == nil {
		return nil, apperr.New(apperr.CodeInternal, "LightweightRegressionDetector: db is nil")
	}

	var maxOffset int64
	if err := d.db.QueryRowContext(ctx, "SELECT COALESCE(MAX(offset), 0) FROM events").Scan(&maxOffset); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "LightweightRegressionDetector: query max offset failed", err)
	}

	recentStart := maxOffset - regressionWindowSize
	baselineStart := maxOffset - 2*regressionWindowSize
	insufficientData := maxOffset < 2*regressionWindowSize

	recentTypes, recentTotal, recentErrCount, err := d.windowStats(ctx, recentStart, maxOffset)
	if err != nil {
		return nil, err
	}

	var baselineTypes map[string]int64
	var baselineTotal, baselineErrCount int64
	if !insufficientData {
		baselineTypes, baselineTotal, baselineErrCount, err = d.windowStats(ctx, baselineStart, recentStart)
		if err != nil {
			return nil, err
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## Lightweight Regression Report for `%s`\n\n", module)
	fmt.Fprintf(&b, "样本窗口：近期 %d 条事件 (offset>%d)，基线 %d 条事件 (offset %d~%d)\n\n",
		recentTotal, recentStart, baselineTotal, baselineStart, recentStart)

	var severity string
	if insufficientData {
		severity = "Warning"
		b.WriteString("### 数据不足\n")
		fmt.Fprintf(&b, "- 历史 EventLog 不足两个对比窗口（每窗口 %d 条），无法进行有效回归对比，建议人工复核。\n", regressionWindowSize)
	} else {
		severity = writeDiffAnalysis(&b, recentTypes, baselineTypes, recentTotal, baselineTotal, recentErrCount, baselineErrCount)
	}

	b.WriteString("\n### Severity\n")
	if severity == "Pass" {
		fmt.Fprintf(&b, "**%s**：未检测到显著回归信号。\n", severity)
	} else {
		fmt.Fprintf(&b, "**%s**：检测到潜在回归信号，请人工复核 EventLog 明细后再批准。\n", severity)
	}

	return &Report{Markdown: b.String(), Severity: severity}, nil
}

// writeDiffAnalysis 向报告写入近期/基线窗口的层间对比明细，返回判定的严重度。
func writeDiffAnalysis(b *strings.Builder, recentTypes, baselineTypes map[string]int64, recentTotal, baselineTotal, recentErrCount, baselineErrCount int64) string {
	b.WriteString("### Layered Diff Analysis（基于 EventLog type 分布 + 错误类 topic 占比，不解码 payload）\n")

	var newTypes []string
	for t := range recentTypes {
		if _, ok := baselineTypes[t]; !ok {
			newTypes = append(newTypes, t)
		}
	}
	sort.Strings(newTypes)
	if len(newTypes) > 0 {
		fmt.Fprintf(b, "- **新增事件类型**：%s（基线窗口未出现，需确认是否为预期行为变更）\n", strings.Join(newTypes, ", "))
	} else {
		b.WriteString("- **事件类型分布**：无新增类型，与基线一致。\n")
	}

	var recentErrRate, baselineErrRate float64
	if recentTotal > 0 {
		recentErrRate = float64(recentErrCount) / float64(recentTotal)
	}
	if baselineTotal > 0 {
		baselineErrRate = float64(baselineErrCount) / float64(baselineTotal)
	}
	fmt.Fprintf(b, "- **错误类事件占比**：近期 %.1f%% (%d/%d) vs 基线 %.1f%% (%d/%d)\n",
		recentErrRate*100, recentErrCount, recentTotal, baselineErrRate*100, baselineErrCount, baselineTotal)

	switch {
	case recentErrCount >= 3 && recentErrRate > baselineErrRate*1.5:
		return "Severe"
	case len(newTypes) > 0 || recentErrRate > baselineErrRate:
		return "Warning"
	default:
		return "Pass"
	}
}
