package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestUsageQueryPath(t *testing.T) {
	p := usageQueryPath("7d", "", "model")
	u, err := url.Parse(p)
	if err != nil || u.Path != "/v1/usage" {
		t.Fatalf("path=%q err=%v", p, err)
	}
	q := u.Query()
	if q.Get("since") != "7d" || q.Get("group_by") != "model" || q.Has("until") {
		t.Fatalf("空 until 不应下发: %v", q)
	}
	// RFC3339 里的 '+' ':' 必须被编码，否则服务端解析成空格。
	p = usageQueryPath("2026-09-30T00:00:00+08:00", "", "purpose")
	if !strings.Contains(p, "%2B08%3A00") {
		t.Fatalf("时间未编码: %s", p)
	}
}

func TestCommaInt(t *testing.T) {
	for in, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -12345: "-12,345"} {
		if got := commaInt(in); got != want {
			t.Errorf("commaInt(%d)=%q want %q", in, got, want)
		}
	}
}

func TestRenderUsageTable(t *testing.T) {
	rep := cliUsageReport{
		Since: "2026-09-29T12:00:00Z", Until: "2026-09-30T12:00:00Z", GroupBy: "purpose",
		Groups: []cliUsageGroup{
			{Key: "plan", Requests: 2, InputTokens: 2000, CacheHitTokens: 1000, CacheHitRatio: 0.5, OutputTokens: 300, ReasoningTokens: 100, CostUSD: 0.6},
			{Key: "graphrag_extract", Requests: 1, ResponseCacheHits: 1},
		},
		Total: cliUsageGroup{Key: "total", Requests: 3, InputTokens: 2000, CacheHitTokens: 1000, CacheHitRatio: 0.5, OutputTokens: 300, ReasoningTokens: 100, CostUSD: 0.6, ResponseCacheHits: 1},
	}
	var buf bytes.Buffer
	renderUsageTable(&buf, rep)
	out := buf.String()
	for _, want := range []string{"分组 purpose", "KEY", "缓存命中", "命中%", "plan", "2,000", "50.0", "0.6000", "graphrag_extract", "total"} {
		if !strings.Contains(out, want) {
			t.Errorf("输出缺少 %q:\n%s", want, out)
		}
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if !strings.Contains(lines[len(lines)-1], "total") {
		t.Errorf("末行应为合计: %q", lines[len(lines)-1])
	}
	// 各数据行列数一致（tabwriter 对齐的前提）。
	header := strings.Fields(lines[2])
	for _, l := range lines[3:] {
		if len(strings.Fields(l)) != len(header) {
			t.Errorf("列数不一致: header=%v line=%q", header, l)
		}
	}
}

func TestRenderUsageTable_Empty(t *testing.T) {
	var buf bytes.Buffer
	renderUsageTable(&buf, cliUsageReport{Since: "a", Until: "b", GroupBy: "day"})
	if !strings.Contains(buf.String(), "没有 LLM 调用记录") {
		t.Fatalf("空报表应给出明确提示: %q", buf.String())
	}
}

// 与守护进程的 JSON 契约对齐：服务端字段名变化时 CLI 解码会漏字段，这里立即暴露。
func TestUsageReportDecodesServerContract(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/usage" || r.URL.Query().Get("group_by") != "day" {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"since":"s","until":"u","group_by":"day","groups":[{"key":"2026-09-30","requests":3,"input_tokens":10,"cache_hit_tokens":4,"output_tokens":2,"reasoning_tokens":1,"cache_hit_ratio":0.4,"cost_usd":0.01,"response_cache_hits":1}],"total":{"key":"total","requests":3}}`))
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL + usageQueryPath("24h", "", "day"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rep cliUsageReport
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	g := rep.Groups[0]
	if g.Key != "2026-09-30" || g.Requests != 3 || g.InputTokens != 10 || g.CacheHitTokens != 4 || g.CacheHitRatio != 0.4 || g.ResponseCacheHits != 1 || g.ReasoningTokens != 1 {
		t.Fatalf("decode: %+v", g)
	}
}

func TestRunUsageCmd_RejectsPositionalArgs(t *testing.T) {
	if err := runUsageCmd([]string{"purpose"}); err == nil {
		t.Fatal("位置参数应被拒绝（避免把 `polaris usage day` 静默当成默认查询）")
	}
}
