// polaris usage 子命令：LLM 用量与缓存命中率报表（ADR-0105 决策八，ADR-0096 决策一）。
//
// 纯 HTTP 客户端：聚合、口径与费用估算全在守护进程侧（GET /v1/usage），CLI 只负责取数与排版，
// 不直连数据库，也不重复实现命中率公式——两份实现只会各自漂移。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// cliUsageGroup / cliUsageReport 对应 GET /v1/usage 的 JSON 契约（sysadmin.UsageReport）。
type cliUsageGroup struct {
	Key               string  `json:"key"`
	Requests          int64   `json:"requests"`
	InputTokens       int64   `json:"input_tokens"`
	CacheHitTokens    int64   `json:"cache_hit_tokens"`
	OutputTokens      int64   `json:"output_tokens"`
	ReasoningTokens   int64   `json:"reasoning_tokens"`
	CacheHitRatio     float64 `json:"cache_hit_ratio"`
	CostUSD           float64 `json:"cost_usd"`
	ResponseCacheHits int64   `json:"response_cache_hits"`
}

type cliUsageReport struct {
	Since   string          `json:"since"`
	Until   string          `json:"until"`
	GroupBy string          `json:"group_by"`
	Groups  []cliUsageGroup `json:"groups"`
	Total   cliUsageGroup   `json:"total"`
}

func runUsageCmd(args []string) error {
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	since := fs.String("since", "24h", "起点：时长（90m/24h/7d，表示多久之前）或 RFC3339 时间")
	until := fs.String("until", "", "终点（默认现在），格式同 --since")
	by := fs.String("by", "purpose", "分组维度：purpose | model | provider | day")
	asJSON := fs.Bool("json", false, "输出原始 JSON")
	fs.Usage = printUsageHelp
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return apperr.Wrap(apperr.CodeInvalidInput, "polaris usage 参数解析失败", err)
	}
	if fs.NArg() > 0 {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("polaris usage 不接受位置参数: %s", strings.Join(fs.Args(), " ")))
	}
	if err := cliCheckServer(); err != nil {
		fmt.Fprintln(os.Stderr, clr(ansiError, "✗ "+err.Error()))
		return err
	}
	var rep cliUsageReport
	if err := cliRequest("GET", usageQueryPath(*since, *until, *by), nil, &rep); err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep) //nolint:wrapcheck // 标准输出写失败无需再包装
	}
	renderUsageTable(os.Stdout, rep)
	return nil
}

func printUsageHelp() {
	fmt.Fprintln(os.Stderr, "用法: polaris usage [--since 24h] [--until <时刻>] [--by purpose|model|provider|day] [--json]")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  --since   起点：多久之前（90m / 24h / 7d）或 RFC3339 时间，默认 24h")
	fmt.Fprintln(os.Stderr, "  --until   终点，格式同 --since，默认现在")
	fmt.Fprintln(os.Stderr, "  --by      分组维度，默认 purpose")
	fmt.Fprintln(os.Stderr, "  --json    输出原始 JSON")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "命中% = 缓存命中输入 token / 全部输入 token；RESP$ 为精确响应缓存命中的调用数（未调用模型，不计 token）。")
}

// usageQueryPath 组装 /v1/usage 查询串；空值不下发，交由服务端取默认。
func usageQueryPath(since, until, by string) string {
	q := url.Values{}
	if since != "" {
		q.Set("since", since)
	}
	if until != "" {
		q.Set("until", until)
	}
	if by != "" {
		q.Set("group_by", by)
	}
	return "/v1/usage?" + q.Encode()
}

// renderUsageTable 把报表排成对齐的文本表，末行为合计。
func renderUsageTable(w io.Writer, rep cliUsageReport) {
	fmt.Fprintf(w, "区间 %s ~ %s（UTC）  分组 %s\n\n", rep.Since, rep.Until, rep.GroupBy)
	if len(rep.Groups) == 0 {
		fmt.Fprintln(w, "该区间内没有 LLM 调用记录。")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, strings.Join([]string{"KEY", "调用", "输入", "缓存命中", "命中%", "输出", "推理", "费用$", "RESP$", ""}, "\t"))
	row := func(g cliUsageGroup) {
		fmt.Fprintln(tw, strings.Join([]string{
			g.Key, commaInt(g.Requests), commaInt(g.InputTokens), commaInt(g.CacheHitTokens),
			fmt.Sprintf("%.1f", g.CacheHitRatio*100), commaInt(g.OutputTokens), commaInt(g.ReasoningTokens),
			fmt.Sprintf("%.4f", g.CostUSD), commaInt(g.ResponseCacheHits), "",
		}, "\t"))
	}
	for _, g := range rep.Groups {
		row(g)
	}
	row(rep.Total)
	tw.Flush() //nolint:errcheck // 写 stdout，失败无可补救
}

// commaInt 千分位。
func commaInt(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
