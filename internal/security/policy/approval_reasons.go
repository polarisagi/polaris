package policy

// 灾难性命令的细分原因码（ADR-0122 决策三、四）。命中时 Decision.Reason 直接用细分码，
// 审批卡据此给出具体说明；历史上的 rm_system_or_home / privilege_escalation / format_filesystem /
// dd_write_device 等仍归并为 ReasonCatastrophicCommand。均为硬限制，任何权限模式都会询问。
const (
	ReasonRmHomeUserDir         = "rm_home_userdir"
	ReasonRmWorkspaceRoot       = "rm_workspace_root"
	ReasonRmGitDir              = "rm_git_dir"
	ReasonRmDynamicRoot         = "rm_dynamic_root"
	ReasonGitDiscardWorktree    = "git_discard_worktree"
	ReasonGitStashDiscard       = "git_stash_discard"
	ReasonGitForcePushProtected = "git_force_push_protected"
	ReasonDiskErase             = "disk_erase"
	ReasonDiskWriteDevice       = "disk_write_device"
	ReasonSecurityPosture       = "security_posture"
	ReasonForkBomb              = "fork_bomb"
	ReasonChmodRecursiveSystem  = "chmod_recursive_system"
	ReasonObfuscatedExec        = "obfuscated_exec"
	ReasonSensitiveFragment     = "sensitive_fragment"
	ReasonEmbeddedScriptDestroy = "embedded_script_destroy"
)

// catastropheReason 把 shell 分类的细分原因码映射为 Decision.Reason；不认识的归并为 ReasonCatastrophicCommand。
func catastropheReason(code string) string {
	switch code {
	case ReasonRmHomeUserDir, ReasonRmWorkspaceRoot, ReasonRmGitDir, ReasonRmDynamicRoot,
		ReasonGitDiscardWorktree, ReasonGitStashDiscard, ReasonGitForcePushProtected,
		ReasonDiskErase, ReasonDiskWriteDevice, ReasonSecurityPosture, ReasonForkBomb,
		ReasonChmodRecursiveSystem, ReasonObfuscatedExec, ReasonSensitiveFragment, ReasonEmbeddedScriptDestroy:
		return code
	}
	return ReasonCatastrophicCommand
}
