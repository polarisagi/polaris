package protocol

import (
	"context"
	"os"
)

// BlobStore VFS 对外最小 Blob 存储接口。
//
// 设计原则（参照 polaris-agent membrane/provider.go）：
//   - 调用方（memory/sandbox/extension）在自己的 provider.go 中引用此接口
//   - 禁止直接 import vfs 包的 WorkspaceManager 具体 struct
//   - 只暴露 Blob 的最小读写操作
//
// @consumer: memory/store, sandbox, extension/skill, extension/mcp
// @producer: vfs.WorkspaceManager
type BlobStore interface {
	// WriteBlob 将 data 写入 VFS，返回不透明引用 ref（格式: "vfs://<hash>"）。
	WriteBlob(data []byte) (ref string, err error)
	// ReadBlob 通过 ref 读取 Blob 内容。ref 不存在返回 apperr.CodeNotFound。
	ReadBlob(ctx context.Context, ref string) ([]byte, error)
	// DeleteBlob 删除指定 Blob（幂等，不存在不报错）。
	DeleteBlob(ctx context.Context, ref string) error
}

// COWProvider VFS 写时复制（Copy-on-Write）接口，专供沙箱使用。
//
// 工作流：
//  1. PrepareEpoch(epochID) → 为本次执行创建隔离工作目录（源自 VFS 快照的 COW）
//  2. 沙箱在工作目录内执行，所有写操作只影响 COW 副本
//  3. CommitEpoch(epochID) → 将合规写入合并回 VFS（需 Capability Token 授权）
//  4. AbortEpoch(epochID) → 丢弃 COW 副本（任何错误路径）
//
// @consumer: sandbox, tool/sandbox
// @producer: vfs.WorkspaceManager
type COWProvider interface {
	// PrepareEpoch 为 epochID 创建 COW 隔离工作目录，返回绝对路径。
	PrepareEpoch(epochID string) (workDir string, err error)
	// CommitEpoch 将 COW 副本中的合规写入合并回主 VFS。
	CommitEpoch(epochID string) error
	// AbortEpoch 丢弃 COW 副本（不回写）。幂等，安全调用多次。
	AbortEpoch(epochID string) error
	// OpenFile 在 VFS 安全上下文中打开文件（限制在允许路径范围内）。
	OpenFile(path string, flag int, perm os.FileMode) (*os.File, error)
}

// VFSFacade 组合 BlobStore + COWProvider 的统一接口。
// 大多数调用方只需要其中一个接口，直接使用细粒度接口更好。
// VFSFacade 供需要两者的模块（如 skill executor）使用。
type VFSFacade interface {
	BlobStore
	COWProvider
}
