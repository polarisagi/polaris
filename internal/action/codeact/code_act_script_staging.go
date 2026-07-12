package codeact

import (
	"os"

	"github.com/google/uuid"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/pkg/apperr"
)

// ScriptStagingBackend 消费端窄接口（R1.4）：CodeAct 执行 LLM 生成代码前，将其
// 落盘为真实临时文件供 OS 级沙箱（Rust bwrap/Seatbelt CmdRunner）bind-mount/
// 读取所需的最小能力，由 *vfs.WorkspaceManager 结构化满足（Go 隐式接口，无需
// vfs 包反向依赖 action/codeact）。
//
// 批次4 XR-11 finding 复核修复：原实现恒定调用 os.CreateTemp("", ...) 落盘到
// 系统级 /tmp，脱离 VFS 隔离边界与配额核算（HE-6）。沙箱执行器本身要求宿主
// 文件系统上的真实路径，无法回避落盘这一步，因此系统最优方案不是"完全不碰
// 文件系统"，而是把落盘目标迁移到 VFS 管辖的 rootDir 之下，纳入既有配额/
// 孤儿清理体系。nil 时降级为 os.CreateTemp（未接入 VFS 的最小 Tier-0 部署/
// 单元测试场景），行为与修复前完全一致。
type ScriptStagingBackend interface {
	StageEphemeralFile(namespace, filename string, data []byte) (absPath string, cleanup func(), err error)
}

// WithScriptStagingBackend 注入 VFS 落盘后端（通常为 *vfs.WorkspaceManager）。
// 未注入时 stageScript 降级为 os.CreateTemp（写入系统临时目录，不纳入 VFS
// 配额/GC 体系），与修复前行为一致。
func WithScriptStagingBackend(b ScriptStagingBackend) CodeActOption {
	return func(c *CodeAct) {
		c.stagingBackend = b
	}
}

// scriptExt 依语言返回脚本文件后缀。
func scriptExt(language string) string {
	switch language {
	case "python":
		return ".py"
	case "bash":
		return ".sh"
	default:
		return ".tmp"
	}
}

// stageScript 将实际执行的脚本字节落盘为真实文件，返回路径与清理函数。
// 优先走 ca.stagingBackend（VFS 隔离边界内、配额受控、崩溃兜底清理）；
// 未注入时降级为包级 writeTempScript（系统 /tmp，向后兼容）。
//
// namespace 优先取 SessionID（同一会话的多次 CodeAct 调用聚在同一子目录，
// 便于排障时按会话查看），为空时退化为 AgentID，再为空时退化为固定值
// ——三者均不参与安全判定，仅影响物理路径分组，SessionID/AgentID 本身
// 已在 vfs.WorkspaceManager.StageEphemeralFile 内部做路径穿越净化。
func (ca *CodeAct) stageScript(req protocol.CodeActRequest, code string) (path string, cleanup func(), err error) {
	if ca.stagingBackend == nil {
		tmpFile, werr := writeTempScript(req.Language, code)
		if werr != nil {
			return "", nil, werr
		}
		return tmpFile, func() { os.Remove(tmpFile) }, nil
	}

	namespace := req.SessionID
	if namespace == "" {
		namespace = req.AgentID
	}
	if namespace == "" {
		namespace = "anon"
	}
	filename := uuid.New().String() + scriptExt(req.Language)

	absPath, cleanupFn, stageErr := ca.stagingBackend.StageEphemeralFile(namespace, filename, []byte(code))
	if stageErr != nil {
		return "", nil, apperr.Wrap(apperr.CodeInternal, "code_act: stage script via VFS backend failed", stageErr)
	}
	return absPath, cleanupFn, nil
}
