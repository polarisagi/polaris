package bash

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/internal/tool/builtin/sandboxenv"
	toolsb "github.com/polarisagi/polaris/internal/tool/sandbox"
	"github.com/polarisagi/polaris/pkg/apperr"
)

type bashArgs struct {
	Command    string `json:"command"`
	Background bool   `json:"background,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
}

// MakeBashFn 创建 bash 工具执行函数。
//
// 执行环境由本次调用的授权凭据派生（ADR-0121 决策三）：显式授权（GrantExplicit，即完全访问、
// 已记住的授权或用户审批）且 pol 未强制容器时在宿主执行、不限路径、允许后台作业与持久终端、
// 工具内命令风险 HITL 让位；其余在沙箱内受 allowedPaths 约束。pol.ForceContainerSandbox
// （hardened 档）覆盖一切，强制容器沙箱。
//
// allowedPaths/sandboxEnabled/netPolicy/bwrapPath 来自 SandboxConfig。
//
//nolint:gocyclo
func MakeBashFn(
	allowedPaths []string,
	sandboxEnabled bool,
	netPolicy protocol.SandboxNetworkPolicy,
	bwrapPath string,
	pol policy.Policy,
	persistentSbx ...*sandbox.PersistentSandbox,
) sandbox.InProcessFn {
	var pSbx *sandbox.PersistentSandbox
	if len(persistentSbx) > 0 {
		pSbx = persistentSbx[0]
	}
	if pol.IsZero() {
		pol = policy.ProfilePolicy(policy.ProfileStandard)
	}
	return func(ctx context.Context, input []byte) ([]byte, error) {
		// ADR-0097 决策五：项目会话追加项目工作目录为可访问根（会话级，不改进程级白名单）。
		paths := guard.ScopedPaths(ctx, allowedPaths)
		var args bashArgs
		if err := json.Unmarshal(input, &args); err != nil {
			return nil, apperr.Wrap(apperr.CodeInternal, "bash: invalid args", err)
		}
		if args.Command == "" {
			return nil, apperr.New(apperr.CodeInternal, "bash: command is required")
		}

		verdictClass, err := guardUngrantedCommand(ctx, args.Command, paths)
		if err != nil {
			return nil, err
		}

		// 显式授权且未被 hardened 强制容器 → 宿主执行（ADR-0121 决策三）
		host := protocol.PermissionGrantFrom(ctx) == protocol.GrantExplicit && !pol.ForceContainerSandbox

		workDir := ""
		// 宿主执行不限路径，workDir 仍取第一个可用路径（通常是项目目录）
		if len(paths) > 0 {
			workDir = paths[0]
		}

		// 1. 后台进程模式（WP5 ADR-0115 / R2-02 / R3-08）：
		// - 宿主执行（显式授权）：直接在宿主机运行；
		// - 其余：后台命令必须走沙箱执行器，若无沙箱支持则明确拒绝，不得回落到宿主；
		// - hardened 档（!pol.BackgroundJobsAllowed）：拒绝后台模式。
		if args.Background {
			if !pol.BackgroundJobsAllowed {
				return nil, apperr.New(apperr.CodeForbidden, "bash: background jobs are disabled in hardened trust profile")
			}
			if !host {
				return nil, apperr.New(apperr.CodeForbidden, "background jobs require explicit authorization (full-access mode, a remembered approval or user approval) or a sandbox backend that supports detached execution")
			}
			return execBackgroundJob(ctx, args.Command, args.SessionID, workDir)
		}

		// 2. 持久终端模式（WP5 ADR-0115 / R2-02 / R3-08）：
		// - hardened 档（!pol.PersistentTerminalAllowed）：拒绝持久终端模式；
		// - 非宿主执行：若无持久沙箱可用，明确拒绝，不得在宿主直接执行；
		// - 宿主执行（显式授权）：允许宿主持久会话。
		if args.SessionID != "" {
			if !pol.PersistentTerminalAllowed {
				return nil, apperr.New(apperr.CodeForbidden, "bash: persistent terminal is disabled in hardened trust profile")
			}
			if !host && (pSbx == nil || !pSbx.Available()) {
				return nil, apperr.New(apperr.CodeForbidden, "persistent terminal requires explicit authorization (full-access mode, a remembered approval or user approval) or a sandbox backend that supports persistent sessions")
			}
			return execPersistentTerminal(ctx, pSbx, args, paths, workDir, host)
		}

		execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		// 按 Policy 决定实际沙箱行为
		effectiveSandbox := sandboxEnabled
		if host {
			// 显式授权：宿主直接执行，忽略配置的 sandboxEnabled
			effectiveSandbox = false
		} else if pol.ForceContainerSandbox {
			// hardened 档：强制容器，覆盖 sandboxEnabled=false
			effectiveSandbox = true
		}

		slog.Info("bash: executing command",
			"sandbox_enabled", effectiveSandbox,
			"network", netPolicy,
			"risk", verdictClass.String(),
			"cmd", args.Command,
			"dir", workDir,
			"profile", pol.Profile,
			"grant", protocol.PermissionGrantFrom(ctx))

		var outBytes []byte
		var execErr error
		var sandboxMethod string

		if effectiveSandbox {
			// V2 统一沙箱接口：env preset 在 Rust 侧由 CallerType=builtin 推导，
			// 凭据过滤由 Rust 侧 CREDENTIAL_STRIP 规则保证（无需 Go 侧 sandboxenv.BaseEnv() 过滤）。
			sandboxCtx := protocol.SandboxContext{
				CallerType:    protocol.CallerBuiltin,
				Command:       args.Command,
				Workdir:       workDir,
				AllowedPaths:  paths,
				NetworkPolicy: ToSandboxNetPolicy(netPolicy),
				BwrapPath:     bwrapPath, // Linux 用户自定义 bwrap 路径（空=自动查找）
				TimeoutMs:     30_000,
			}
			var setupErr error
			outBytes, execErr, sandboxMethod, setupErr = ExecInSandbox(execCtx, sandboxCtx)
			if setupErr != nil {
				return nil, apperr.Wrap(apperr.CodeInternal, "bash: sandbox wrap failed", setupErr)
			}
		} else {
			// 沙箱禁用：env 清理 + workDir + Linux namespace（最后防线）
			sandboxMethod = "disabled"
			outBytes, execErr = ExecWithoutSandbox(execCtx, args.Command, workDir, sandboxenv.BaseEnv())
		}

		result := map[string]any{
			"command":         args.Command,
			"output":          string(outBytes),
			"exit_code":       0,
			"sandbox_enabled": effectiveSandbox,
			"sandbox_method":  sandboxMethod,
			"network_policy":  netPolicy,
		}
		if execErr != nil {
			result["error"] = execErr.Error()
			var exitErr *exec.ExitError
			if errors.As(execErr, &exitErr) {
				result["exit_code"] = exitErr.ExitCode()
			} else {
				result["exit_code"] = -1
			}
		}
		return json.Marshal(result)
	}
}

func ExecInSandbox(_ context.Context, sandboxCtx protocol.SandboxContext) ([]byte, error, string, error) {
	resp, setupErr := toolsb.RustSandboxExecV2(sandboxCtx, sandboxCtx.TimeoutMs)
	if setupErr != nil {
		return nil, nil, "", setupErr //nolint:wrapcheck
	}
	var cmdErr error
	if resp.ExitCode != 0 {
		cmdErr = apperr.New(apperr.CodeInternal, fmt.Sprintf("exit status %d", resp.ExitCode))
	}
	return []byte(resp.Output), cmdErr, resp.SandboxMethod, nil
}

// ExecWithoutSandbox 在沙箱已被上游显式禁用（sandboxEnabled==false）时的最后防线执行路径。
// XR-10 豁免说明：这是 RunSandboxedArgv/ExecInSandbox 不可用时的降级分支本身（调用方已经
// 决定跳过沙箱），因此这里不能再次委托沙箱包装（会形成递归降级/无意义包装）；安全性由
// 调用方负责（env 清理 + workDir 限制 + Linux namespace 隔离，见调用点 bash.go:93 注释），
// 本函数只做裸执行。
func ExecWithoutSandbox(ctx context.Context, command, workDir string, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.Dir = workDir
	cmd.Env = env
	return cmd.CombinedOutput() //nolint:wrapcheck
}

func ToSandboxNetPolicy(p protocol.SandboxNetworkPolicy) string {
	if p == protocol.NetPolicyAllow {
		return protocol.NetPolicyAllow
	}
	return protocol.NetPolicyDeny
}

// guardUngrantedCommand 无授权凭据（未经 policy.Decide 的兜底路径）时用唯一的 shell 分析器 fail-closed：
// ≥ 危险一律拒绝，需先经权限判定/审批拿到凭据（ADR-0122 决策一）。已有凭据说明 Decide 已判定过，不再分级。
func guardUngrantedCommand(ctx context.Context, cmd string, roots []string) (policy.CmdClass, error) {
	if protocol.PermissionGrantFrom(ctx) != protocol.GrantNone {
		return policy.CmdSafe, nil
	}
	home, _ := os.UserHomeDir()
	v := policy.ClassifyCommand(cmd, policy.CmdEnv{Roots: roots, Home: home})
	if v.Class >= policy.CmdDangerous {
		slog.Error("bash: ungranted command rejected", "cmd", cmd, "class", v.Class.String(), "reason", v.Reason)
		return v.Class, apperr.New(apperr.CodeForbidden,
			fmt.Sprintf("bash: command requires permission approval (%s: %s); run it through the permission decision first", v.Class, v.Reason))
	}
	return v.Class, nil
}

func execBackgroundJob(ctx context.Context, cmdStr, sessionID, workDir string) ([]byte, error) {
	sessID := sessionID
	if sessID == "" {
		sessID, _ = guard.SessionIDFromCtx(ctx)
	}
	jobMeta, err := GetDefaultJobManager().StartJob(
		ctx,
		cmdStr,
		workDir,
		sessID,
		sandboxenv.BaseEnv(),
	)
	if err != nil {
		return nil, err
	}
	result := map[string]any{
		"command":    cmdStr,
		"job_id":     jobMeta.JobID,
		"status":     jobMeta.Status,
		"started_at": jobMeta.StartedAt,
		"session_id": jobMeta.SessionID,
		"background": true,
	}
	out, err := json.Marshal(result)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "bash: marshal background result", err)
	}
	return out, nil
}

func execPersistentTerminal(
	ctx context.Context,
	pSbx *sandbox.PersistentSandbox,
	args bashArgs,
	paths []string,
	workDir string,
	host bool,
) ([]byte, error) {
	if pSbx == nil {
		return nil, apperr.New(apperr.CodeUnimplemented, "bash: persistent terminal backend not configured")
	}
	spec := sandbox.SandboxSpec{
		ToolName:      "bash",
		Language:      "bash",
		SessionID:     args.SessionID,
		HostExecution: host,
		AllowedPaths:  paths,
		WorkDir:       workDir,
		ScriptBytes:   []byte(args.Command),
		ExtraEnv:      sandboxenv.BaseEnv(),
	}
	toolRes, err := pSbx.Run(ctx, spec)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "bash: persistent session run", err)
	}
	result := map[string]any{
		"command":    args.Command,
		"output":     string(toolRes.Output),
		"exit_code":  0,
		"session_id": args.SessionID,
		"persistent": true,
	}
	if !toolRes.Success {
		result["exit_code"] = -1
		if toolRes.Error != "" {
			result["error"] = toolRes.Error
		}
	}
	out, err := json.Marshal(result)
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeInternal, "bash: marshal persistent result", err)
	}
	return out, nil
}
