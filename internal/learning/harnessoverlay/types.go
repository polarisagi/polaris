package harnessoverlay

import (
	"context"
	"time"
)

// Version 表示 harness 覆盖层 git 仓库的一个版本提交记录。
type Version struct {
	Hash         string    `json:"hash"`
	Author       string    `json:"author"`
	Timestamp    time.Time `json:"timestamp"`
	Message      string    `json:"message"`
	ChangedFiles []string  `json:"changed_files,omitempty"`
}

// HarnessOverlayManager 管理 <DataDir>/harness_overlay/ 只读/可回滚覆盖层。
// 唯一可编辑面：skills/、prompt_addenda/、tool_notes/。
// 评测器配置、信任档位与内核源码绝对不在可编辑面内（ADR-0115 WP6）。
type HarnessOverlayManager interface {
	// Dir 返回覆盖层物理根目录绝对路径。
	Dir() string
	// Init 初始化覆盖层目录结构并建立内部 git 仓库（幂等）。
	Init(ctx context.Context) error
	// ValidatePath 校验待写入路径是否属于合法覆盖面。
	ValidatePath(relPath string) error
	// WriteAndCommit 将文件改动写入覆盖层并提交一个版本，返回 commit hash。
	WriteAndCommit(ctx context.Context, files map[string][]byte, message string, author string) (string, error)
	// History 查询覆盖层版本历史。
	History(ctx context.Context, limit int) ([]Version, error)
	// Rollback 将覆盖层代码与提示词硬回滚至指定 commit hash。
	Rollback(ctx context.Context, version string) error
	// GetPromptAddenda 读取全部 prompt_addenda/*.md 并按文件名确定序拼接，供稳定层末尾注入。
	GetPromptAddenda(ctx context.Context) (string, error)
	// GetToolNotes 读取全部 tool_notes/*.md，返回 toolName -> 说明补充 的字典。
	GetToolNotes(ctx context.Context) (map[string]string, error)
	// GetToolNote 读取特定工具的说明补充，不存在时返回空串。
	GetToolNote(ctx context.Context, toolName string) (string, error)
}
