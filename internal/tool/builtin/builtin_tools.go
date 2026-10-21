package builtin

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/polarisagi/polaris/internal/tool"

	_ "modernc.org/sqlite" // data_query 工具的 SQLite 驱动（ADR-0003：纯 Go，无 CGO）

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/llm/modelcatalog"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/security/policy"
	"github.com/polarisagi/polaris/internal/tool/builtin/agent_message"
	"github.com/polarisagi/polaris/internal/tool/builtin/agent_result"
	"github.com/polarisagi/polaris/internal/tool/builtin/agent_status"
	"github.com/polarisagi/polaris/internal/tool/builtin/agent_stop"
	"github.com/polarisagi/polaris/internal/tool/builtin/bash"
	"github.com/polarisagi/polaris/internal/tool/builtin/call_tool"
	"github.com/polarisagi/polaris/internal/tool/builtin/checkpoint_list"
	"github.com/polarisagi/polaris/internal/tool/builtin/checkpoint_restore"
	"github.com/polarisagi/polaris/internal/tool/builtin/csv_parse"
	"github.com/polarisagi/polaris/internal/tool/builtin/data_query"
	"github.com/polarisagi/polaris/internal/tool/builtin/diff_text"
	"github.com/polarisagi/polaris/internal/tool/builtin/execute_wasm"
	"github.com/polarisagi/polaris/internal/tool/builtin/feed_fetch"
	"github.com/polarisagi/polaris/internal/tool/builtin/get_datetime"
	"github.com/polarisagi/polaris/internal/tool/builtin/get_task_result"
	"github.com/polarisagi/polaris/internal/tool/builtin/glob"
	"github.com/polarisagi/polaris/internal/tool/builtin/goal"
	"github.com/polarisagi/polaris/internal/tool/builtin/grep"
	"github.com/polarisagi/polaris/internal/tool/builtin/job_kill"
	"github.com/polarisagi/polaris/internal/tool/builtin/job_output"
	"github.com/polarisagi/polaris/internal/tool/builtin/job_status"
	"github.com/polarisagi/polaris/internal/tool/builtin/list_dir"
	"github.com/polarisagi/polaris/internal/tool/builtin/list_models"
	"github.com/polarisagi/polaris/internal/tool/builtin/multi_edit"
	"github.com/polarisagi/polaris/internal/tool/builtin/notebook_edit"
	"github.com/polarisagi/polaris/internal/tool/builtin/notebook_read"
	"github.com/polarisagi/polaris/internal/tool/builtin/read_file"
	"github.com/polarisagi/polaris/internal/tool/builtin/read_tool_ref"
	"github.com/polarisagi/polaris/internal/tool/builtin/run_command"
	"github.com/polarisagi/polaris/internal/tool/builtin/secret_get"
	"github.com/polarisagi/polaris/internal/tool/builtin/session_read"
	"github.com/polarisagi/polaris/internal/tool/builtin/session_search"
	"github.com/polarisagi/polaris/internal/tool/builtin/spawn_agents"
	"github.com/polarisagi/polaris/internal/tool/builtin/str_replace_editor"
	"github.com/polarisagi/polaris/internal/tool/builtin/switch_model"
	"github.com/polarisagi/polaris/internal/tool/builtin/sys_probe"
	"github.com/polarisagi/polaris/internal/tool/builtin/todo_read"
	"github.com/polarisagi/polaris/internal/tool/builtin/todo_write"
	ttstool "github.com/polarisagi/polaris/internal/tool/builtin/tts"
	"github.com/polarisagi/polaris/internal/tool/builtin/video_analysis"
	"github.com/polarisagi/polaris/internal/tool/builtin/web_fetch"
	"github.com/polarisagi/polaris/internal/tool/builtin/web_search"
	"github.com/polarisagi/polaris/internal/tool/builtin/write_file"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

type HITLGateway interface {
	RequestApproval(ctx context.Context, action string, req map[string]any) error
}

func RegisterBuiltinTools(
	sbx *sandbox.InProcessSandbox,
	toolReg *tool.InMemoryToolRegistry,
	allowedPaths []string, // 文件系统路径白名单（read_file/list_dir/write_file 均受限）
	dialer protocol.SafeDialer,
	sandboxEnabled bool, // 是否启用平台原生进程沙箱
	netPolicy protocol.SandboxNetworkPolicy, // bash/run_command 网络访问策略
	bwrapPath string, // Linux: bwrap 路径（空=自动查找）
	cfg *config.Config,
	autoSvc AutomationService, // automation_* 工具依赖；nil 时不注册这四个工具
	vfsRoot string, // WorkspaceManager 的根目录
	asyncTaskProvider get_task_result.AsyncTaskProvider, // get_task_result 工具依赖（GD-08-001）；nil 时该工具始终降级返回 expired_or_not_found
	hitlGateway HITLGateway,
	todoRepo repo.TodoRepository, // todo_write/todo_read 按会话落库（ADR-0104 决策三）；nil 时两工具调用即报错
	ttsSynth ttstool.Synthesizer, // tts 工具依赖的本地 TTS 引擎（ADR-0107）；nil 时该工具调用即报错，不回出假音频
	webDeps web_fetch.Deps, // web_fetch 工具依赖（ADR-0114 决策三）
	pol policy.Policy, // 信任档位安全策略快照（ADR-0115 WP1）；影响 bash/run_command 沙箱行为
	// full 档专属工具依赖（ADR-0115 WP1）
	checkpointDataDir string, // 快照仓库数据根目录；pol.CheckpointEnabled=true 时必须非空
	checkpointProjects []string, // 被快照的项目目录列表（通常是 allowedPaths 的第一个）
	secretReader secret_get.NamedSecretReader, // nil = 不注册 secret_get
	// list_models 工具依赖（ADR-0115 WP3）
	db protocol.SQLQuerier,
	modelCatalogMgr *modelcatalog.CatalogManager,
	persistentSbx ...*sandbox.PersistentSandbox,
) error {
	var pSbx *sandbox.PersistentSandbox
	if len(persistentSbx) > 0 {
		pSbx = persistentSbx[0]
	}

	if webDeps.Dialer == nil {
		webDeps.Dialer = dialer
	}
	if webDeps.Limiter == nil {
		webDeps.Limiter = web_fetch.NewHostLimiter(func() time.Duration {
			return time.Duration(config.CurrentThresholds().M7Tool.WebFetchHostMinIntervalMs) * time.Millisecond
		})
	}
	if pol.IsZero() {
		pol = policy.ProfilePolicy(policy.ProfileStandard)
	}

	// 元数据与实现绑定表：name → InProcessFn
	// 元数据从 builtin/<name>/tool.yaml + schema.json 加载，不再硬编码在此处。
	defs := append(make([]builtinDef, 0, 48), []builtinDef{
		{"read_file", read_file.MakeReadFileFn(allowedPaths)},
		{"list_dir", list_dir.MakeListDirFn(allowedPaths)},
		{"write_file", write_file.MakeWriteFileFn(allowedPaths)},
		{"web_fetch", web_fetch.MakeWebFetchFn(webDeps)},
		{"feed_fetch", feed_fetch.MakeFeedFetchFn(webDeps)},
		{"bash", bash.MakeBashFn(allowedPaths, sandboxEnabled, netPolicy, bwrapPath, hitlGateway, pol, pSbx)},
		{"run_command", run_command.MakeRunCommandFn(allowedPaths, sandboxEnabled, netPolicy, bwrapPath, hitlGateway, pol)},
		{"get_datetime", get_datetime.GetDatetimeFn},
		{"csv_parse", csv_parse.CsvParseFn},
		{"diff_text", diff_text.DiffTextFn},
		{"video_analysis", video_analysis.MakeExecuteVideoAnalysisFn(allowedPaths, dialer, sandboxEnabled, bwrapPath)},
		{"tts", ttstool.MakeTTSFn(ttsSynth)},
		{"sys_probe", sys_probe.SysProbeFn},
		{"str_replace_editor", str_replace_editor.MakeStrReplaceEditorFn(allowedPaths)},
		{"read_tool_ref", read_tool_ref.MakeReadToolRefFn(vfsRoot)},
		{"glob", glob.MakeGlobFn(allowedPaths)},
		{"web_search", web_search.MakeWebSearchFn(cfg, dialer, webDeps.Limiter)},
		{"todo_write", todo_write.MakeTodoWriteFn(todoRepo)},
		{"todo_read", todo_read.MakeTodoReadFn(todoRepo)},
		{"multi_edit", multi_edit.MakeMultiEditFn(allowedPaths)},
		{"notebook_read", notebook_read.MakeNotebookReadFn(allowedPaths)},
		{"notebook_edit", notebook_edit.MakeNotebookEditFn(allowedPaths)},
		{"grep", grep.MakeGrepFn(allowedPaths)},

		{"data_query", data_query.MakeDataQueryFn(allowedPaths)},

		// git_diff/git_commit/template_render 实现见 git_text_tools.go
		// （与本文件同属 package builtin，无需导入）；三者此前只有空壳
		// metadata-less 占位文件（GR-5-008），已核实功能实现完整但从未
		// 注册进 defs，导致 `make deadcode` 一直靠白名单掩盖——现补齐
		// tool.yaml/schema.json（见 git_diff/、git_commit/、
		// template_render/ 三个目录）并在此接线。
		{"git_diff", MakeGitDiffFn(allowedPaths, sandboxEnabled, bwrapPath)},
		{"git_commit", MakeGitCommitFn(allowedPaths, sandboxEnabled, bwrapPath)},
		{"template_render", TemplateRenderFn},

		// 模型自选与目录工具（ADR-0115 WP3）
		{"list_models", list_models.MakeListModelsFn(db, modelCatalogMgr)},
		// 跨会话对话检索（ADR-0120 决策六），只读。
		{"session_search", session_search.MakeSessionSearchFn(db)},
		{"session_read", session_read.MakeSessionReadFn(db)},
		{"switch_model", switch_model.MakeSwitchModelFn()},

		// 并行子智能体与控制工具（ADR-0115 WP4）
		{"spawn_agents", spawn_agents.MakeSpawnAgentsFn()},
		{"agent_status", agent_status.MakeAgentStatusFn()},
		{"agent_result", agent_result.MakeAgentResultFn()},
		{"agent_message", agent_message.MakeAgentMessageFn()},
		{"agent_stop", agent_stop.MakeAgentStopFn()},

		// 后台任务管理工具（ADR-0115 WP5）
		{"job_status", job_status.MakeJobStatusFn(bash.GetDefaultJobManager())},
		{"job_output", job_output.MakeJobOutputFn(bash.GetDefaultJobManager())},
		{"job_kill", job_kill.MakeJobKillFn(bash.GetDefaultJobManager())},

		// 历史内激活的元工具（ADR-0120 决策三）：真正的分发在内核解包，这里只登记元数据与直调兜底。
		{"call_tool", call_tool.CallToolFn},
	}...)

	defs = append(defs, automationToolDefs(autoSvc)...)
	defs = append(defs, fullProfileToolDefs(pol, checkpointDataDir, checkpointProjects)...)

	for _, d := range defs {
		meta, err := tool.GetBuiltinToolMeta(d.name)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal,
				fmt.Sprintf("builtin_tools: load meta for %q", d.name), err)
		}
		applyDescriptionOverride(&meta, pol)
		sbx.Register(meta.Name, d.fn)
		if err := toolReg.Register(meta); err != nil {
			return apperr.Wrap(apperr.CodeInternal,
				fmt.Sprintf("builtin_tools: register %q", d.name), err)
		}
	}

	if pol.SecretGetEnabled && secretReader != nil {
		maxPerTurn := config.CurrentThresholds().M4Kernel.SecretGetMaxPerTurn
		meta, err := tool.GetBuiltinToolMeta("secret_get")
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "builtin_tools: load meta for secret_get", err)
		}
		sbx.RegisterWithTaint(meta.Name, secret_get.MakeSecretGetFn(secretReader, maxPerTurn), types.TaintHigh)
		if err := toolReg.Register(meta); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "builtin_tools: register secret_get", err)
		}
	}

	richDefs := []struct {
		name string
		fn   sandbox.InProcessRichFn
	}{
		{"execute_wasm", execute_wasm.MakeExecuteWasmFn(allowedPaths)},
		{"get_task_result", get_task_result.MakeGetTaskResultFn(asyncTaskProvider)},
	}

	for _, d := range richDefs {
		meta, err := tool.GetBuiltinToolMeta(d.name)
		if err != nil {
			slog.Warn("builtin_tools: skipped tool (missing metadata)", "tool", d.name, "err", err)
			continue
		}
		sbx.RegisterRich(meta.Name, d.fn, types.TaintHigh)
		if err := toolReg.Register(meta); err != nil {
			return apperr.Wrap(apperr.CodeInternal,
				fmt.Sprintf("builtin_tools: register %q", d.name), err)
		}
	}

	return nil
}

type builtinDef struct {
	name string
	fn   sandbox.InProcessFn
}

func automationToolDefs(autoSvc AutomationService) []builtinDef {
	if autoSvc == nil {
		return nil
	}
	return []builtinDef{
		{"automation_list", MakeAutomationListFn(autoSvc)},
		{"automation_save", MakeAutomationSaveFn(autoSvc)},
		{"automation_run_now", MakeAutomationRunNowFn(autoSvc)},
		{"automation_delete", MakeAutomationDeleteFn(autoSvc)},
	}
}

func fullProfileToolDefs(
	pol policy.Policy,
	checkpointDataDir string,
	checkpointProjects []string,
) []builtinDef {
	var out []builtinDef
	if pol.CheckpointEnabled && checkpointDataDir != "" && len(checkpointProjects) > 0 {
		projectDir := checkpointProjects[0]
		retentionCount := config.CurrentThresholds().M4Kernel.CheckpointRetentionCount
		out = append(out, []builtinDef{
			{"checkpoint_list", checkpoint_list.MakeCheckpointListFn(checkpointDataDir, projectDir, retentionCount)},
			{"checkpoint_restore", checkpoint_restore.MakeCheckpointRestoreFn(checkpointDataDir, projectDir, retentionCount)},
		}...)
	}
	return out
}

// applyDescriptionOverride 对随运行配置变化的工具描述做启动期覆盖。
func applyDescriptionOverride(meta *types.Tool, pol policy.Policy) {
	if meta.Name == "bash" {
		meta.Description = bashDescription(pol)
	}
}

// bashDescription 按信任档位给出 bash 的真实执行环境（ADR-0120 决策三第 7 条）。描述在启动时确定、
// 整个进程不变（tools 数组字节稳定）；此前一律写"sandboxed、限定工作目录"，full 档下与事实不符，
// 模型会因此回避本可以直接做的事。
func bashDescription(pol policy.Policy) string {
	const tail = " background=true runs it as a job (see job_status/job_output); session_id keeps a persistent shell across calls."
	if pol.SandboxDisabled && pol.AllowedPathsOpen {
		return "Run a shell command directly on the host." + tail
	}
	return "Run a shell command in the sandbox, restricted to the allowed working directories." + tail
}

// RegisterGoalTool 注册跨回合目标工具（ADR-0120 决策七）。单独成函数以免继续膨胀
// RegisterBuiltinTools 的签名；goals 为 nil 时不注册（该工具调用无意义）。
func RegisterGoalTool(sbx *sandbox.InProcessSandbox, toolReg *tool.InMemoryToolRegistry, goals repo.GoalRepository) error {
	if goals == nil {
		return nil
	}
	meta, err := tool.GetBuiltinToolMeta("goal")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "builtin_tools: load meta for goal", err)
	}
	sbx.Register(meta.Name, goal.MakeGoalFn(goals))
	if err := toolReg.Register(meta); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "builtin_tools: register goal", err)
	}
	return nil
}
