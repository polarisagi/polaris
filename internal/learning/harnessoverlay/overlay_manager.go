package harnessoverlay

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/polarisagi/polaris/pkg/apperr"
)

var gitHashPattern = regexp.MustCompile(`^[0-9a-fA-F]{4,40}$`)

// Manager 实现 HarnessOverlayManager。
type Manager struct {
	dir string
	mu  sync.RWMutex
}

// New 创建 Manager 实例。dir 必须为绝对路径。
func New(dir string) *Manager {
	return &Manager{
		dir: filepath.Clean(dir),
	}
}

// Dir 返回覆盖层物理根目录。
func (m *Manager) Dir() string {
	return m.dir
}

// Init 初始化覆盖层目录结构并建立内部 git 仓库（幂等）。
func (m *Manager) Init(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	subdirs := []string{
		m.dir,
		filepath.Join(m.dir, "skills"),
		filepath.Join(m.dir, "prompt_addenda"),
		filepath.Join(m.dir, "tool_notes"),
	}
	for _, d := range subdirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "harnessoverlay.Init: mkdir failed", err)
		}
	}

	gitDir := filepath.Join(m.dir, ".git")
	if _, err := os.Stat(gitDir); err == nil {
		return nil
	}

	if err := m.execGit(ctx, "init"); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "harnessoverlay.Init: git init failed", err)
	}
	if err := m.execGit(ctx, "config", "user.name", "Polaris Self-Improvement"); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "harnessoverlay.Init: git config user.name failed", err)
	}
	if err := m.execGit(ctx, "config", "user.email", "polaris@local"); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "harnessoverlay.Init: git config user.email failed", err)
	}

	for _, sub := range []string{"skills", "prompt_addenda", "tool_notes"} {
		keepFile := filepath.Join(m.dir, sub, ".gitkeep")
		if err := os.WriteFile(keepFile, []byte{}, 0o600); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "harnessoverlay.Init: write .gitkeep failed", err)
		}
	}

	if err := m.execGit(ctx, "add", "."); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "harnessoverlay.Init: git add failed", err)
	}
	if err := m.execGit(ctx, "commit", "-m", "chore: initial harness overlay", "--author=Polaris Self-Improvement <polaris@local>"); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "harnessoverlay.Init: initial commit failed", err)
	}
	return nil
}

// ValidatePath 校验待写入路径是否属于合法覆盖面。
// 仅允许 skills/、prompt_addenda/*.md、tool_notes/*.md。
func (m *Manager) ValidatePath(relPath string) error {
	clean := filepath.Clean(relPath)
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") || clean == "." {
		return apperr.New(apperr.CodeForbidden, fmt.Sprintf("harnessoverlay: invalid relative path: %s", relPath))
	}

	parts := strings.Split(clean, string(filepath.Separator))
	if len(parts) < 2 {
		return apperr.New(apperr.CodeForbidden, fmt.Sprintf("harnessoverlay: path must be inside a subdirectory: %s", relPath))
	}

	top := parts[0]
	switch top {
	case "skills":
		return nil
	case "prompt_addenda", "tool_notes":
		if filepath.Ext(clean) != ".md" {
			return apperr.New(apperr.CodeForbidden, fmt.Sprintf("harnessoverlay: %s only accepts .md files", top))
		}
		return nil
	default:
		return apperr.New(apperr.CodeForbidden, fmt.Sprintf("harnessoverlay: directory %q is not in editable surface", top))
	}
}

// WriteAndCommit 将文件改动写入覆盖层并提交一个版本，返回 commit hash。
func (m *Manager) WriteAndCommit(ctx context.Context, files map[string][]byte, message string, author string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if len(files) == 0 {
		return "", apperr.New(apperr.CodeInvalidInput, "harnessoverlay: no files to commit")
	}

	// 1. 全量预校验，一旦有非法路径立即拒绝（fail-fast）
	for relPath := range files {
		if err := m.ValidatePath(relPath); err != nil {
			return "", err
		}
	}

	// 2. 写入物理文件
	for relPath, content := range files {
		absPath := filepath.Join(m.dir, relPath)
		if err := os.MkdirAll(filepath.Dir(absPath), 0o700); err != nil {
			return "", apperr.Wrap(apperr.CodeInternal, "harnessoverlay: mkdir failed", err)
		}
		if err := os.WriteFile(absPath, content, 0o600); err != nil {
			return "", apperr.Wrap(apperr.CodeInternal, "harnessoverlay: write file failed", err)
		}
	}

	// 3. 执行 git add 与 commit
	if err := m.execGit(ctx, "add", "."); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "harnessoverlay: git add failed", err)
	}

	if author == "" {
		author = "Polaris Self-Improvement <polaris@local>"
	}
	if message == "" {
		message = "feat(overlay): auto-improve harness"
	}

	if err := m.execGit(ctx, "commit", "-m", message, "--author="+author); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "harnessoverlay: git commit failed", err)
	}

	// 4. 获取新 commit hash
	out, err := m.outputGit(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "harnessoverlay: rev-parse HEAD failed", err)
	}
	return strings.TrimSpace(out), nil
}

// History 查询覆盖层版本历史。
func (m *Manager) History(ctx context.Context, limit int) ([]Version, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if limit <= 0 {
		limit = 20
	}
	out, err := m.outputGit(ctx, "log", fmt.Sprintf("-n%d", limit), "--format=%H%x00%an%x00%at%x00%s")
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "harnessoverlay: git log failed", err)
	}

	lines := strings.Split(strings.TrimSpace(out), "\n")
	versions := make([]Version, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\x00")
		if len(parts) < 4 {
			continue
		}
		tsInt, _ := strconv.ParseInt(parts[2], 10, 64)
		versions = append(versions, Version{
			Hash:      parts[0],
			Author:    parts[1],
			Timestamp: time.Unix(tsInt, 0),
			Message:   parts[3],
		})
	}
	return versions, nil
}

// Rollback 将覆盖层代码与提示词硬回滚至指定 commit hash。
func (m *Manager) Rollback(ctx context.Context, version string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	version = strings.TrimSpace(version)
	if !gitHashPattern.MatchString(version) {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("harnessoverlay: invalid git commit hash: %s", version))
	}

	if err := m.execGit(ctx, "reset", "--hard", version); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "harnessoverlay: git reset failed", err)
	}
	return nil
}

// GetPromptAddenda 读取全部 prompt_addenda/*.md 并按文件名确定序拼接，供稳定层末尾注入。
func (m *Manager) GetPromptAddenda(ctx context.Context) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	dir := filepath.Join(m.dir, "prompt_addenda")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", apperr.Wrap(apperr.CodeInternal, "harnessoverlay: read prompt_addenda dir", err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var sb strings.Builder
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		trimmed := strings.TrimSpace(string(b))
		if trimmed == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(trimmed)
	}
	return sb.String(), nil
}

// GetToolNotes 读取全部 tool_notes/*.md，返回 toolName -> 说明补充 的字典。
func (m *Manager) GetToolNotes(ctx context.Context) (map[string]string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	dir := filepath.Join(m.dir, "tool_notes")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]string), nil
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "harnessoverlay: read tool_notes dir", err)
	}

	notes := make(map[string]string)
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			toolName := strings.TrimSuffix(e.Name(), ".md")
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			trimmed := strings.TrimSpace(string(b))
			if trimmed != "" {
				notes[toolName] = trimmed
			}
		}
	}
	return notes, nil
}

// GetToolNote 读取特定工具的说明补充，不存在时返回空串。
func (m *Manager) GetToolNote(ctx context.Context, toolName string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	p := filepath.Join(m.dir, "tool_notes", toolName+".md")
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", apperr.Wrap(apperr.CodeInternal, "harnessoverlay: read tool note", err)
	}
	return strings.TrimSpace(string(b)), nil
}

func (m *Manager) execGit(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = m.dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("git %s: %s", strings.Join(args, " "), string(out)), err)
	}
	return nil
}

func (m *Manager) outputGit(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = m.dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("git %s failed", strings.Join(args, " ")), err)
	}
	return string(out), nil
}
