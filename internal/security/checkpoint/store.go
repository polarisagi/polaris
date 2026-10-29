// Package checkpoint 提供影子 git 写前快照能力（ADR-0115 WP1）。
//
// 快照模型：
//   - 每个项目目录对应一个影子 git 仓库，路径为 <DataDir>/checkpoints/<project_hash>.git
//   - 使用 `git --git-dir=<shadow> --work-tree=<projectDir>` 操作，不影响项目本身的 .git
//   - 触发条件：full 档下，写类操作 + 高风险操作前，同一回合最多 CheckpointThrottleMaxPerTurn 次
//   - 回滚：checkpoint_restore 工具调用 git checkout 恢复到指定快照
//
// 为什么用影子 git：项目目录本身可能无 git 仓库（bare 目录）或有 git 但用户不想被干扰。
// 影子仓库与项目 .git 完全独立，DataDir 有完整的写权限（~/.polaris 是 polaris 自己的数据目录）。
package checkpoint

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/polarisagi/polaris/pkg/apperr"
)

var validCommitHashRe = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// Store 管理一个项目目录的影子 git 快照仓库。
type Store struct {
	// gitDir 影子 git 仓库路径（--git-dir），位于 DataDir/checkpoints/ 下。
	gitDir string
	// workTree 被快照的项目目录（--work-tree）。
	workTree string
	// retentionCount 最多保留的快照数（0 = 不裁剪）。
	retentionCount int
}

// NewStore 创建或打开一个项目目录的影子 git 快照仓库。
//
//   - dataDir: polaris 数据目录（~/.polaris 或配置的 data_dir）
//   - projectDir: 被快照的项目目录（绝对路径）
//   - retentionCount: 保留的最近快照数（0 = 不裁剪）
func NewStore(dataDir, projectDir string, retentionCount int) (*Store, error) {
	// 用项目路径的 SHA-256 前 16 字节作为仓库名，避免路径特殊字符问题。
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(projectDir)))[:16]
	gitDir := filepath.Join(dataDir, "checkpoints", hash+".git")

	s := &Store{
		gitDir:         gitDir,
		workTree:       projectDir,
		retentionCount: retentionCount,
	}
	if err := s.init(); err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "checkpoint: init shadow repo", err)
	}
	return s, nil
}

// Create 在当前工作树状态创建一个快照，返回 commit hash。
//
// message 应简要描述触发快照的操作（如 "pre: write_file /foo/bar.go"）。
// 若工作树无变更，跳过提交并返回最近一次 commit hash（幂等）。
func (s *Store) Create(ctx context.Context, message string) (string, error) {
	ctx, span := otel.Tracer("polaris/checkpoint").Start(ctx, "checkpoint.Create")
	defer span.End()
	span.SetAttributes(
		attribute.String("work_tree", s.workTree),
		attribute.String("message", message),
	)

	// add -A：把新增/修改/删除都纳入暂存区
	if err := s.git(ctx, "add", "-A"); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "checkpoint: git add", err)
	}

	// 检查是否有变更需要提交（干净树直接返回 HEAD hash）
	statusOut, err := s.gitOut(ctx, "status", "--porcelain")
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "checkpoint: git status", err)
	}
	if strings.TrimSpace(string(statusOut)) == "" {
		// 工作树无变更，返回 HEAD
		return s.headHash(ctx)
	}

	ts := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	fullMsg := fmt.Sprintf("[polaris-checkpoint %s] %s", ts, message)
	if err := s.git(ctx, "commit", "--no-verify", "-m", fullMsg); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "checkpoint: git commit", err)
	}

	hash, err := s.headHash(ctx)
	if err != nil {
		return "", err
	}
	slog.Info("checkpoint: snapshot created", "hash", hash, "work_tree", s.workTree, "msg", message)
	span.SetAttributes(attribute.String("commit_hash", hash))

	// 超出保留数时裁剪旧快照
	if s.retentionCount > 0 {
		if pErr := s.prune(ctx); pErr != nil {
			slog.Warn("checkpoint: prune failed", "err", pErr)
		}
	}
	return hash, nil
}

// List 返回最近 n 条快照（commit hash + 消息），按时间倒序。
// n <= 0 时返回全部。
func (s *Store) List(ctx context.Context, n int) ([]Snapshot, error) {
	ctx, span := otel.Tracer("polaris/checkpoint").Start(ctx, "checkpoint.List")
	defer span.End()

	args := []string{"log", "--format=%H|%s|%ci", "--no-walk=unsorted"}
	if n > 0 {
		args = append(args, fmt.Sprintf("-n%d", n))
	} else {
		args = append(args, "--all")
	}
	out, err := s.gitOut(ctx, args...)
	if err != nil {
		// 新建仓库无 commit 时 git log 返回 128，视为空列表
		if strings.Contains(err.Error(), "exit status 128") {
			return nil, nil
		}
		return nil, apperr.Wrap(apperr.CodeInternal, "checkpoint: git log", err)
	}

	var snaps []Snapshot
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 2 {
			continue
		}
		snaps = append(snaps, Snapshot{Hash: parts[0], Message: parts[1], Timestamp: safeTime(parts)})
	}
	return snaps, nil
}

// Restore 把工作树恢复到指定快照。
// 恢复采用 git read-tree --reset -u 检出目标快照，并使用 git clean -fd 清理快照后新增的文件与目录（严格遵守 .gitignore）。
// 这是不可逆操作，调用方（checkpoint_restore 工具）应已取得用户确认或 full 档自动许可。
func (s *Store) Restore(ctx context.Context, hash string) error {
	ctx, span := otel.Tracer("polaris/checkpoint").Start(ctx, "checkpoint.Restore")
	defer span.End()
	span.SetAttributes(
		attribute.String("hash", hash),
		attribute.String("work_tree", s.workTree),
	)

	if !validCommitHashRe.MatchString(hash) {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("checkpoint: invalid commit hash format: %q", hash))
	}
	if err := s.git(ctx, "cat-file", "-e", hash+"^{commit}"); err != nil {
		return apperr.Wrap(apperr.CodeNotFound, fmt.Sprintf("checkpoint: commit not found: %s", hash), err)
	}

	// 先做一次快照，保留恢复前状态
	if _, sErr := s.Create(ctx, fmt.Sprintf("pre-restore: before restoring to %s", hash[:min8(hash)])); sErr != nil {
		slog.Warn("checkpoint: pre-restore backup failed", "err", sErr)
	}

	if err := s.git(ctx, "read-tree", "--reset", "-u", hash); err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("checkpoint: restore read-tree to %s", hash), err)
	}
	if err := s.git(ctx, "clean", "-fd"); err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("checkpoint: restore clean for %s", hash), err)
	}
	slog.Info("checkpoint: restored", "hash", hash, "work_tree", s.workTree)
	return nil
}

// Snapshot 快照摘要。
type Snapshot struct {
	Hash      string
	Message   string
	Timestamp time.Time
}

// ── 内部辅助 ──────────────────────────────────────────────────────────────────

func (s *Store) init() error {
	if err := os.MkdirAll(s.gitDir, 0o700); err != nil {
		return apperr.Wrap(apperr.CodeInternal, fmt.Sprintf("mkdir %s", s.gitDir), err)
	}
	// --bare=false + --work-tree：影子仓库模式
	cmd := exec.Command("git",
		"--git-dir="+s.gitDir,
		"--work-tree="+s.workTree,
		"init", "-q",
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "git init", err)
	}
	// 设置 user 避免 commit 报错（不继承全局 git config，保持隔离）
	if err := exec.Command("git", "--git-dir="+s.gitDir, "config", "user.email", "polaris-checkpoint@local").Run(); err != nil {
		slog.Warn("checkpoint: init git config user.email failed", "err", err)
	}
	if err := exec.Command("git", "--git-dir="+s.gitDir, "config", "user.name", "polaris-checkpoint").Run(); err != nil {
		slog.Warn("checkpoint: init git config user.name failed", "err", err)
	}
	return nil
}

func (s *Store) git(ctx context.Context, args ...string) error {
	fullArgs := append([]string{"--git-dir=" + s.gitDir, "--work-tree=" + s.workTree}, args...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	cmd.Stderr = os.Stderr
	return cmd.Run() //nolint:wrapcheck
}

func (s *Store) gitOut(ctx context.Context, args ...string) ([]byte, error) {
	fullArgs := append([]string{"--git-dir=" + s.gitDir, "--work-tree=" + s.workTree}, args...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	return cmd.Output() //nolint:wrapcheck
}

func (s *Store) headHash(ctx context.Context) (string, error) {
	out, err := s.gitOut(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "checkpoint: rev-parse HEAD", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// prune 裁剪超出 retentionCount 的旧 commit（让 branch 指向保留窗口的最新 commit，旧 commit 被 gc）。
func (s *Store) prune(ctx context.Context) error {
	out, err := s.gitOut(ctx, "log", "--format=%H", fmt.Sprintf("-n%d", s.retentionCount+1))
	if err != nil {
		_ = err
		return nil //nolint:nilerr // expected when shadow repo has no commits yet
	}
	hashes := strings.Fields(string(out))
	if len(hashes) <= s.retentionCount {
		return nil // 未超出，无需裁剪
	}
	// 用 grafts 机制截断历史：让 retentionCount-th commit 无父节点
	cutoff := hashes[s.retentionCount-1]
	if err := s.git(ctx, "replace", "--graft", cutoff); err != nil {
		slog.Warn("checkpoint: prune graft failed", "err", err)
	}
	if err := s.git(ctx, "gc", "--prune=now", "-q"); err != nil {
		slog.Warn("checkpoint: prune gc failed", "err", err)
	}
	return nil
}

func safeTime(parts []string) time.Time {
	if len(parts) < 3 {
		return time.Time{}
	}
	t, _ := time.Parse("2006-01-02 15:04:05 -0700", parts[2])
	return t
}

func min8(s string) int {
	if len(s) < 8 {
		return len(s)
	}
	return 8
}
