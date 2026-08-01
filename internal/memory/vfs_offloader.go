package memory

import (
	"context"
	"errors"
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
	wm WorkspaceProvider // 构造时注入；消费端窄接口，不持有 *vfs.WorkspaceManager 具体类型
}

// NewToolRefOffloader 构造 ToolRefOffloader
func NewToolRefOffloader(db protocol.SQLQuerier, wm WorkspaceProvider) *ToolRefOffloader {
	return &ToolRefOffloader{
		db: db,
		wm: wm,
	}
}

// Offload 将 content 写入 taskID 的隔离工作区 tool_refs/ 子目录，
// 登记 workspace_vfs 行，返回可被 read_tool_ref(task_id, id) 读回的 id。
func (o *ToolRefOffloader) Offload(ctx context.Context, taskID string, content []byte) (string, error) {
	// 配额检查（预占式，D-B6-01）：拒绝超过 Tier0 工作区配额的写入，防止工具
	// 输出无限增长打满磁盘。阶段03 R-07：CheckQuota/ReleaseQuota 裸配对改用
	// WithQuota 闭包收敛——fn 返回 error 时自动归还预占份额；数据库插入失败
	// 分支刻意返回 nil（见内部注释），文件已写入磁盘占用实际空间，不能让
	// WithQuota 在此自动释放，否则配额与磁盘实际占用不一致。
	size := int64(len(content))
	var id string
	var dbErr error
	qerr := o.wm.WithQuota(size, func() error {
		// 1. 获取任务隔离目录（经 WorkspaceManager 保证不越权）
		if _, err := o.wm.Create(taskID); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "ToolRefOffloader: failed to create workspace dir", err)
		}

		// 2. 生成 UUID 存根 ID
		id = uuid.New().String()

		// 3. 构建相对路径与绝对路径
		relPath := filepath.Join(taskID, "tool_refs", id+".log")
		fullPath := filepath.Join(o.wm.GetRootDir(), relPath)

		// 写入文件
		if err := o.wm.WriteFile(relPath, content); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "ToolRefOffloader: failed to write tool ref file", err)
		}

		// 4. 插入 workspace_vfs 表
		query := `
			INSERT INTO workspace_vfs(id, task_id, file_path, size, meta, created_at)
			VALUES (?, ?, ?, ?, NULL, ?)
		`
		if _, err := o.db.ExecContext(ctx, query, id, taskID, relPath, len(content), time.Now().Unix()); err != nil {
			// 数据库失败无法轻易回滚文件，但在 Workspace 目录里会被 GC 掉；
			// 文件确已写入磁盘占用了实际空间。先 RegisterFile 让 quota/GC 正常
			// 感知该文件（与成功路径一致），再把 DB 错误记到外层变量、本闭包
			// 返回 nil——按 GC 迟早回收该孤儿文件时再通过 GC 路径归还，避免
			// WithQuota 因 fn 返回 error 而在此处误释放配额。
			o.wm.RegisterFile(taskID, vfs.WorkspaceFile{Path: fullPath, Size: int64(len(content))})
			dbErr = apperr.Wrap(apperr.CodeInternal, "ToolRefOffloader: failed to insert workspace_vfs", err)
			return nil
		}

		// 5. 让 WorkspaceManager 的 quota/GC 感知此文件
		o.wm.RegisterFile(taskID, vfs.WorkspaceFile{
			Path: fullPath,
			Size: int64(len(content)),
		})
		return nil
	})
	if qerr != nil {
		if errors.Is(qerr, vfs.ErrWorkspaceQuotaExhausted) {
			return "", apperr.Wrap(apperr.CodeResourceExhausted, "ToolRefOffloader: workspace quota exceeded", qerr)
		}
		// fn 内部（Create/WriteFile 失败）已构造好 apperr.Error；经 WithQuota
		// 接口方法边界回传时 wrapcheck 要求显式包一层——用 apperr.CodeOf(qerr)
		// 透传原始 Code，而非固定 Code，避免掩盖 fn 内部已分类好的错误语义
		// （同 R-06 skill/plugin 生成器的 apperr.Wrap(apperr.CodeOf(...)) 惯例，
		// 参见 internal/extension/marketplace/manager.go:150）。
		return "", apperr.Wrap(apperr.CodeOf(qerr), "ToolRefOffloader: workspace write failed", qerr)
	}
	if dbErr != nil {
		return "", dbErr
	}

	return id, nil
}
