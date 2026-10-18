package protocol

import (
	"context"
	"time"
)

// HarnessVersion 表示覆盖层 git 提交版本。
type HarnessVersion struct {
	Hash         string    `json:"hash"`
	Author       string    `json:"author"`
	Timestamp    time.Time `json:"timestamp"`
	Message      string    `json:"message"`
	ChangedFiles []string  `json:"changed_files,omitempty"`
}

// HarnessOverlayManager 管理 <DataDir>/harness_overlay/ Git 仓库与覆盖面文件。
type HarnessOverlayManager interface {
	Dir() string
	Init(ctx context.Context) error
	ValidatePath(relPath string) error
	WriteAndCommit(ctx context.Context, files map[string][]byte, message string, author string) (commitHash string, err error)
	History(ctx context.Context, limit int) ([]HarnessVersion, error)
	Rollback(ctx context.Context, commitHash string) error
	GetPromptAddenda(ctx context.Context) (string, error)
	GetToolNotes(ctx context.Context) (map[string]string, error)
	GetToolNote(ctx context.Context, toolName string) (string, error)
}
