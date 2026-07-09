package grep

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type grepArgs struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path"`
	Glob            string `json:"glob"`
	OutputMode      string `json:"output_mode"`
	ContextBefore   int    `json:"context_before"`
	ContextAfter    int    `json:"context_after"`
	CaseInsensitive bool   `json:"case_insensitive"`
	HeadLimit       int    `json:"head_limit"`
}

type grepMatch struct {
	File          string   `json:"file"`
	Line          int      `json:"line"`
	Text          string   `json:"text"`
	ContextBefore []string `json:"context_before,omitempty"`
	ContextAfter  []string `json:"context_after,omitempty"`
}

type grepRunner struct {
	re        *regexp.Regexp
	args      grepArgs
	mode      string
	limit     int
	matches   []grepMatch
	files     []string
	counts    []grepFileCount
	total     int
	truncated bool
	seenFiles map[string]struct{}
}

func (g *grepRunner) walk(path string, d os.DirEntry, walkErr error) error {
	if walkErr != nil {
		return nil //nolint:nilerr // 目录项读取失败时静默跳过，不中断整体 walk
	}
	if d.IsDir() {
		return nil
	}
	if g.truncated {
		return filepath.SkipAll
	}
	if g.args.Glob != "" {
		if matched, _ := doublestar.Match(g.args.Glob, filepath.Base(path)); !matched {
			return nil
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil //nolint:nilerr // 权限不足等情况静默跳过
	}
	if isBinaryData(data) {
		return nil
	}
	return g.scanFile(path, strings.Split(string(data), "\n"))
}

func (g *grepRunner) scanFile(path string, lines []string) error {
	matchCount := 0
	hasMatch := false
	for i, line := range lines {
		if !g.re.MatchString(line) {
			continue
		}
		matchCount++
		hasMatch = true
		if err := g.handleMatch(path, i, line, lines); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "grepRunner.scanFile", err) // filepath.SkipAll 会向上传递至 WalkDir
		}
		if g.mode == "files_with_matches" {
			break // 每文件只记录一次，无需扫描剩余行
		}
	}
	return g.postFile(path, matchCount, hasMatch)
}

func (g *grepRunner) handleMatch(path string, i int, line string, lines []string) error {
	switch g.mode {
	case "content":
		g.matches = append(g.matches, g.buildMatch(path, i, line, lines))
		if len(g.matches) >= g.limit {
			g.truncated = true
			return filepath.SkipAll
		}
	case "files_with_matches":
		if _, seen := g.seenFiles[path]; !seen {
			g.seenFiles[path] = struct{}{}
			g.files = append(g.files, path)
		}
	}
	return nil
}

func (g *grepRunner) postFile(path string, matchCount int, hasMatch bool) error {
	if g.mode == "files_with_matches" && len(g.files) >= g.limit {
		g.truncated = true
		return filepath.SkipAll
	}
	if g.mode == "count" && hasMatch {
		g.total += matchCount
		g.counts = append(g.counts, grepFileCount{File: path, Count: matchCount})
		if len(g.counts) >= g.limit {
			g.truncated = true
			return filepath.SkipAll
		}
	}
	return nil
}

func (g *grepRunner) buildMatch(path string, i int, line string, lines []string) grepMatch {
	m := grepMatch{File: path, Line: i + 1, Text: line}
	if g.args.ContextBefore > 0 {
		start := i - g.args.ContextBefore
		if start < 0 {
			start = 0
		}
		m.ContextBefore = lines[start:i]
	}
	if g.args.ContextAfter > 0 {
		end := i + 1 + g.args.ContextAfter
		if end > len(lines) {
			end = len(lines)
		}
		m.ContextAfter = lines[i+1 : end]
	}
	return m
}

func (g *grepRunner) result() ([]byte, error) {
	switch g.mode {
	case "content":
		return json.Marshal(map[string]any{"matches": g.matches, "truncated": g.truncated}) //nolint:wrapcheck
	case "files_with_matches":
		return json.Marshal(map[string]any{"files": g.files, "truncated": g.truncated}) //nolint:wrapcheck
	case "count":
		return json.Marshal(map[string]any{"counts": g.counts, "total": g.total, "truncated": g.truncated}) //nolint:wrapcheck
	default:
		return nil, apperr.New(apperr.CodeInternal, "grep: unreachable")
	}
}

func MakeGrepFn(allowedPaths []string) sandbox.InProcessFn {
	return func(_ context.Context, input []byte) ([]byte, error) {
		var args grepArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "grep: invalid args", err)
		}
		if args.Pattern == "" {
			return nil, apperr.New(apperr.CodeInternal, "grep: pattern is required")
		}
		if len(allowedPaths) == 0 {
			return nil, apperr.New(apperr.CodeInternal, "grep: no allowed paths configured")
		}
		if err := grepValidateMode(args.OutputMode); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "makeGrepFn", err)
		}

		reStr := args.Pattern
		if args.CaseInsensitive {
			reStr = "(?i)" + reStr
		}
		re, err := regexp.Compile(reStr)
		if err != nil {
			return nil, apperr.New(apperr.CodeInternal, fmt.Sprintf("grep: invalid pattern: %v", err))
		}

		searchRoots := allowedPaths
		if args.Path != "" {
			if err := guard.CheckAllowedPath(args.Path, allowedPaths); err != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "makeGrepFn", err)
			}
			searchRoots = []string{filepath.Clean(args.Path)}
		}

		grepClampArgs(&args)
		runner := newGrepRunner(re, args)

		for _, root := range searchRoots {
			if walkErr := filepath.WalkDir(filepath.Clean(root), runner.walk); walkErr != nil {
				slog.Warn("grep: walk error", "root", root, "err", walkErr)
			}
			if runner.truncated {
				break
			}
		}

		return runner.result()
	}
}

func newGrepRunner(re *regexp.Regexp, args grepArgs) *grepRunner {
	mode := args.OutputMode
	if mode == "" {
		mode = "files_with_matches"
	}
	limit := args.HeadLimit
	if limit <= 0 {
		limit = 250
	}
	if limit > 1000 {
		limit = 1000
	}
	return &grepRunner{
		re:        re,
		args:      args,
		mode:      mode,
		limit:     limit,
		seenFiles: make(map[string]struct{}),
	}
}

func grepClampArgs(args *grepArgs) {
	if args.ContextBefore < 0 {
		args.ContextBefore = 0
	}
	if args.ContextAfter < 0 {
		args.ContextAfter = 0
	}
	if args.ContextBefore > 10 {
		args.ContextBefore = 10
	}
	if args.ContextAfter > 10 {
		args.ContextAfter = 10
	}
}

func grepValidateMode(mode string) error {
	switch mode {
	case "", "content", "files_with_matches", "count":
		return nil
	default:
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("grep: unknown output_mode %q", mode))
	}
}

func isBinaryData(data []byte) bool {
	probe := data
	if len(probe) > 512 {
		probe = probe[:512]
	}
	for _, b := range probe {
		if b == 0 {
			return true
		}
	}
	return false
}

type grepFileCount struct {
	File  string `json:"file"`
	Count int    `json:"count"`
}
