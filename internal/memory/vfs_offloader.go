package memory

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/vfs"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ToolRefOffloader 将超限工具输出落盘到任务隔离工作区，并登记 workspace_vfs 索引，
// 供 read_tool_ref 工具与 SemanticCompressHandler 双路径按需读回（M05 §11.3 Stage 1）。
type ToolRefOffloader struct {
	db protocol.SQLQuerier
	wm *vfs.WorkspaceManager // 构造时注入
}

// NewToolRefOffloader 构造 ToolRefOffloader
func NewToolRefOffloader(db protocol.SQLQuerier, wm *vfs.WorkspaceManager) *ToolRefOffloader {
	return &ToolRefOffloader{
		db: db,
		wm: wm,
	}
}

// Offload 将 content 写入 taskID 的隔离工作区 tool_refs/ 子目录，
// 登记 workspace_vfs 行，返回可被 read_tool_ref(task_id, id) 读回的 id。
func (o *ToolRefOffloader) Offload(ctx context.Context, taskID string, content []byte) (string, error) {
	// 1. 获取任务隔离目录（经 WorkspaceManager 保证不越权）
	_, err := o.wm.Create(taskID)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "ToolRefOffloader: failed to create workspace dir", err)
	}

	// 2. 生成 UUID 存根 ID
	id := uuid.New().String()

	// 3. 构建相对路径与绝对路径
	relPath := filepath.Join(taskID, "tool_refs", id+".log")
	fullPath := filepath.Join(o.wm.GetRootDir(), relPath)

	// 写入文件
	if err := os.MkdirAll(filepath.Dir(fullPath), 0700); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "ToolRefOffloader: failed to mkdir tool_refs", err)
	}
	if err := os.WriteFile(fullPath, content, 0600); err != nil {
		return "", apperr.Wrap(apperr.CodeInternal, "ToolRefOffloader: failed to write tool ref file", err)
	}

	// 4. 插入 workspace_vfs 表
	query := `
		INSERT INTO workspace_vfs(id, task_id, file_path, size, meta, created_at)
		VALUES (?, ?, ?, ?, NULL, ?)
	`
	_, err = o.db.ExecContext(ctx, query, id, taskID, relPath, len(content), time.Now().Unix())
	if err != nil {
		// 数据库失败需回滚文件，避免孤儿文件（尽力而为）
		_ = os.Remove(fullPath)
		return "", apperr.Wrap(apperr.CodeInternal, "ToolRefOffloader: failed to insert workspace_vfs", err)
	}

	// 5. 让 WorkspaceManager 的 quota/GC 感知此文件
	o.wm.RegisterFile(taskID, vfs.WorkspaceFile{
		Path: fullPath,
		Size: int64(len(content)),
	})

	return id, nil
}
