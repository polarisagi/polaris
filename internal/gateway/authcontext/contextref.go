package authcontext

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

// ─── 配置常量 ───────────────────────────────────────────────────────────────────

const (
	defaultMaxExpandTokens = 32000   // 所有引用展开后的 token 软上限
	maxSingleFileBytes     = 1 << 20 // 单个文件上限 1MB
	maxSingleURLBytes      = 1 << 19 // 单个 URL 上限 512KB
	httpFetchTimeout       = 15 * time.Second
)

// sensitiveBlockedPaths 敏感路径前缀——命中则拒绝展开。
func sensitiveBlockedPaths() []string {
	return []string{
		".ssh", ".aws", ".kube", ".docker",
		"id_rsa", ".npmrc", ".netrc",
		".config/gcloud", ".config/gh",
		"credentials", ".env",
		".git/config",
	}
}

// ─── 引用模式 ───────────────────────────────────────────────────────────────────

// ExpandReport 展开操作的结果报告。
type ExpandReport struct {
	TotalBytes  int               // 展开后总字节数
	TokenBudget int               // 当前 token 上限
	OverBudget  bool              // 是否超预算（展开结果被截断）
	Skipped     []string          // 被跳过/拒绝的引用（敏感路径等）
	Resolved    map[string]string // 引用原文 → 展开摘要（诊断用）
}

// ContextRefExpander 消息引用展开器。
// 扫描用户输入中的 @file / @url 引用，异步并发展开后替换回原文。
type ContextRefExpander struct {
	client          *http.Client
	maxExpandTokens int
	workDir         string // 文件路径解析基准目录（空 = CWD）
	refPattern      *regexp.Regexp
}

// NewContextRefExpander 创建展开器。
func NewContextRefExpander(client *http.Client, opts ...func(*ContextRefExpander)) *ContextRefExpander {
	e := &ContextRefExpander{
		client:          client,
		maxExpandTokens: defaultMaxExpandTokens,
		refPattern:      regexp.MustCompile(`@(file|url|git):"([^"]*)"`),
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// WithMaxExpandTokens 设置展开 token 上限。
func WithMaxExpandTokens(n int) func(*ContextRefExpander) {
	return func(e *ContextRefExpander) { e.maxExpandTokens = n }
}

// WithWorkDir 设置文件读取的基准目录。
func WithWorkDir(dir string) func(*ContextRefExpander) {
	return func(e *ContextRefExpander) { e.workDir = dir }
}

// ─── 敏感路径检测 ──────────────────────────────────────────────────────────────

func isSensitivePath(path string) bool {
	clean := filepath.Clean(path)
	for _, blocked := range sensitiveBlockedPaths() {
		if matched, _ := filepath.Match(blocked, clean); matched {
			return true
		}
		if strings.Contains(clean, string(filepath.Separator)+blocked) {
			return true
		}
		if strings.HasPrefix(clean, blocked+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// ─── 展开主流程 ────────────────────────────────────────────────────────────────

// Expand 扫描并展开文本中的 @file / @url 引用。
// 返回展开后的文本和操作报告。
func (e *ContextRefExpander) Expand(ctx context.Context, text string) (string, *ExpandReport) {
	matches := e.refPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return text, &ExpandReport{TokenBudget: e.maxExpandTokens}
	}

	report := &ExpandReport{
		TokenBudget: e.maxExpandTokens,
		Resolved:    make(map[string]string),
	}

	// 去重：相同原文只展开一次
	type matchKey struct{ typ, val string }
	type rawMatch struct{ typ, val, raw string }
	seen := make(map[matchKey]bool)
	var uniqueMatches []rawMatch //nolint:prealloc
	for _, m := range matches {
		if len(m) < 3 {
			continue
		}
		k := matchKey{typ: m[1], val: m[2]}
		if seen[k] {
			continue
		}
		seen[k] = true
		uniqueMatches = append(uniqueMatches, rawMatch{typ: m[1], val: m[2], raw: m[0]})
	}

	// 并发展开唯一引用
	type result struct {
		raw     string
		content string
		err     string
		bytes   int
	}
	ch := make(chan result, len(uniqueMatches))
	var wg sync.WaitGroup

	for _, m := range uniqueMatches {
		wg.Add(1)
		raw, typ, val := m.raw, m.typ, m.val
		concurrent.SafeGo(ctx, "gateway.authcontext.expand_ref", func(ctx context.Context) {
			defer wg.Done()
			r := result{raw: raw}

			if typ == "file" && isSensitivePath(val) {
				r.err = "blocked: sensitive path"
				ch <- r
				return
			}

			content, bytes, err := e.resolveOne(ctx, typ, val)
			if err != nil {
				r.err = err.Error()
				ch <- r
				return
			}
			r.content = content
			r.bytes = bytes
			ch <- r
		})
	}
	wg.Wait()
	close(ch)

	// 收集结果
	type replacement struct {
		raw     string
		content string
	}
	var replacements []replacement //nolint:prealloc
	totalBytes := 0

	for r := range ch {
		if r.err != "" {
			report.Skipped = append(report.Skipped, fmt.Sprintf("%s (%s)", r.raw, r.err))
			report.Resolved[r.raw] = "skipped: " + r.err
			continue
		}
		totalBytes += r.bytes
		replacements = append(replacements, replacement{raw: r.raw, content: r.content})
		report.Resolved[r.raw] = fmt.Sprintf("expanded (%d bytes)", r.bytes)
	}

	report.TotalBytes = totalBytes

	// 检查 token 预算（粗略估算 bytes/4）
	estimatedTokens := totalBytes / 4
	if estimatedTokens > e.maxExpandTokens {
		report.OverBudget = true
	}

	// 逐引用替换（不包含原文在 replacement 中，避免已替换文本被二次匹配）
	out := text
	for _, rp := range replacements {
		replacement := fmt.Sprintf("\n[以下来自引用]\n%s\n[/引用结束]\n", rp.content)
		out = strings.ReplaceAll(out, rp.raw, replacement)
	}

	return out, report
}

func (e *ContextRefExpander) resolveOne(ctx context.Context, typ, val string) (content string, bytes int, err error) {
	switch typ {
	case "file":
		return e.resolveFile(ctx, val)
	case "url":
		return e.resolveURL(ctx, val)
	case "git":
		return "", 0, apperr.New(apperr.CodeUnimplemented, "git references not yet supported")
	default:
		return "", 0, apperr.New(apperr.CodeInternal, fmt.Sprintf("unknown reference type: %s", typ))
	}
}

// ─── 文件引用解析 ──────────────────────────────────────────────────────────────

func (e *ContextRefExpander) resolveFile(_ context.Context, val string) (string, int, error) { //nolint:gocyclo
	path := val
	var lineStart, lineEnd int

	// 解析行范围 "path.go:10-20" 或 "path.go:10"
	if colonIdx := strings.LastIndexByte(val, ':'); colonIdx > 0 {
		rangePart := val[colonIdx+1:]
		if strings.Contains(rangePart, "-") {
			sp := strings.SplitN(rangePart, "-", 2)
			v1, err1 := strconv.Atoi(strings.TrimSpace(sp[0]))
			v2, err2 := strconv.Atoi(strings.TrimSpace(sp[1]))
			if err1 == nil && err2 == nil && v1 > 0 && v2 >= v1 {
				lineStart, lineEnd = v1, v2
				path = val[:colonIdx]
			}
		} else if v, err := strconv.Atoi(rangePart); err == nil && v > 0 {
			lineStart, lineEnd = v, v
			path = val[:colonIdx]
		}
	}

	// 解析为绝对路径
	if !filepath.IsAbs(path) && e.workDir != "" {
		path = filepath.Join(e.workDir, path)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return "", 0, apperr.Wrap(apperr.CodeInternal, "resolve path", err)
	}
	if isSensitivePath(abs) {
		return "", 0, apperr.New(apperr.CodeInternal, fmt.Sprintf("blocked: sensitive path %q", abs))
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return "", 0, apperr.Wrap(apperr.CodeInternal, "read file", err)
	}
	if len(data) > maxSingleFileBytes {
		data = data[:maxSingleFileBytes]
	}

	// 行范围裁剪
	if lineEnd > 0 {
		lines := strings.Split(string(data), "\n")
		if lineStart > len(lines) {
			return "", 0, apperr.New(apperr.CodeInternal, fmt.Sprintf("line %d exceeds file length %d", lineStart, len(lines)))
		}
		if lineEnd > len(lines) {
			lineEnd = len(lines)
		}
		data = []byte(strings.Join(lines[lineStart-1:lineEnd], "\n"))
	}

	return string(data), len(data), nil
}

// ─── URL 引用解析 ──────────────────────────────────────────────────────────────

func (e *ContextRefExpander) resolveURL(ctx context.Context, val string) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", val, nil)
	if err != nil {
		return "", 0, apperr.Wrap(apperr.CodeInternal, "bad url", err)
	}
	req.Header.Set("User-Agent", "polaris/1.0")

	client := e.client
	if client == nil {
		// http.DefaultClient 绕过 SafeDialer 的 SSRF 校验，禁止静默降级。
		// 生产路径必须通过 NewContextRefExpander(substrate.NewSafeHTTPClient(...)) 注入。
		return "", 0, apperr.Wrap(apperr.CodeInternal, "http client not injected: use NewSafeHTTPClient", nil)
	}

	fetchCtx, cancel := context.WithTimeout(ctx, httpFetchTimeout)
	defer cancel()
	req = req.WithContext(fetchCtx)

	resp, err := client.Do(req)
	if err != nil {
		return "", 0, apperr.Wrap(apperr.CodeInternal, "fetch url", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", 0, apperr.New(apperr.CodeInternal, fmt.Sprintf("HTTP %d", resp.StatusCode))
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxSingleURLBytes)))
	if err != nil {
		return "", 0, apperr.Wrap(apperr.CodeInternal, "read body", err)
	}

	return string(body), len(body), nil
}
