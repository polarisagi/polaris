package builtin

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/polarisagi/polaris/internal/tool"

	_ "modernc.org/sqlite" // data_query 工具的 SQLite 驱动（ADR-0003：纯 Go，无 CGO）

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/protocol/repo"
	"github.com/polarisagi/polaris/internal/sandbox"
	"github.com/polarisagi/polaris/internal/tool/builtin/bash"
	"github.com/polarisagi/polaris/internal/tool/builtin/csv_parse"
	"github.com/polarisagi/polaris/internal/tool/builtin/data_query"
	"github.com/polarisagi/polaris/internal/tool/builtin/diff_text"
	"github.com/polarisagi/polaris/internal/tool/builtin/execute_wasm"
	"github.com/polarisagi/polaris/internal/tool/builtin/fetch_url"
	"github.com/polarisagi/polaris/internal/tool/builtin/get_datetime"
	"github.com/polarisagi/polaris/internal/tool/builtin/get_task_result"
	"github.com/polarisagi/polaris/internal/tool/builtin/glob"
	"github.com/polarisagi/polaris/internal/tool/builtin/grep"
	"github.com/polarisagi/polaris/internal/tool/builtin/list_dir"
	"github.com/polarisagi/polaris/internal/tool/builtin/multi_edit"
	"github.com/polarisagi/polaris/internal/tool/builtin/notebook_edit"
	"github.com/polarisagi/polaris/internal/tool/builtin/notebook_read"
	"github.com/polarisagi/polaris/internal/tool/builtin/read_file"
	"github.com/polarisagi/polaris/internal/tool/builtin/read_tool_ref"
	"github.com/polarisagi/polaris/internal/tool/builtin/run_command"
	"github.com/polarisagi/polaris/internal/tool/builtin/str_replace_editor"
	"github.com/polarisagi/polaris/internal/tool/builtin/sys_probe"
	"github.com/polarisagi/polaris/internal/tool/builtin/todo_read"
	"github.com/polarisagi/polaris/internal/tool/builtin/todo_write"
	ttstool "github.com/polarisagi/polaris/internal/tool/builtin/tts"
	"github.com/polarisagi/polaris/internal/tool/builtin/video_analysis"
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
	cronRepo protocol.CronRepository, // cron_* 工具依赖；nil 时不注册这三个工具
	vfsRoot string, // WorkspaceManager 的根目录
	asyncTaskProvider get_task_result.AsyncTaskProvider, // get_task_result 工具依赖（GD-08-001）；nil 时该工具始终降级返回 expired_or_not_found
	hitlGateway HITLGateway,
	todoRepo repo.TodoRepository, // todo_write/todo_read 按会话落库（ADR-0104 决策三）；nil 时两工具调用即报错
	ttsSynth ttstool.Synthesizer, // tts 工具依赖的本地 TTS 引擎（ADR-0107）；nil 时该工具调用即报错，不回出假音频
) error {
	// 元数据与实现绑定表：name → InProcessFn
	// 元数据从 builtin/<name>/tool.yaml + schema.json 加载，不再硬编码在此处。
	defs := []struct {
		name string
		fn   sandbox.InProcessFn
	}{
		{"read_file", read_file.MakeReadFileFn(allowedPaths)},
		{"list_dir", list_dir.MakeListDirFn(allowedPaths)},
		{"write_file", write_file.MakeWriteFileFn(allowedPaths)},
		{"fetch_url", fetch_url.MakeFetchURLFn(dialer)},
		{"bash", bash.MakeBashFn(allowedPaths, sandboxEnabled, netPolicy, bwrapPath, hitlGateway)},
		{"run_command", run_command.MakeRunCommandFn(allowedPaths, sandboxEnabled, netPolicy, bwrapPath, hitlGateway)},
		{"get_datetime", get_datetime.GetDatetimeFn},
		{"csv_parse", csv_parse.CsvParseFn},
		{"diff_text", diff_text.DiffTextFn},
		{"video_analysis", video_analysis.MakeExecuteVideoAnalysisFn(allowedPaths, dialer, sandboxEnabled, bwrapPath)},
		{"tts", ttstool.MakeTTSFn(ttsSynth)},
		{"sys_probe", sys_probe.SysProbeFn},
		{"str_replace_editor", str_replace_editor.MakeStrReplaceEditorFn(allowedPaths)},
		{"read_tool_ref", read_tool_ref.MakeReadToolRefFn(vfsRoot)},
		{"glob", glob.MakeGlobFn(allowedPaths)},
		{"web_search", web_search.MakeWebSearchFn(cfg, dialer)},
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
	}

	// cron_* 工具依赖，仅在 cronRepo != nil 时注册（单元测试无 Repo 时不报错）
	if cronRepo != nil {
		defs = append(defs, []struct {
			name string
			fn   sandbox.InProcessFn
		}{
			{"cron_list", MakeCronListFn(cronRepo)},
			{"cron_create", MakeCronCreateFn(cronRepo)},
			{"cron_delete", MakeCronDeleteFn(cronRepo)},
		}...)
	}

	for _, d := range defs {
		meta, err := tool.GetBuiltinToolMeta(d.name)
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal,
				fmt.Sprintf("builtin_tools: load meta for %q", d.name), err)
		}
		sbx.Register(meta.Name, d.fn)
		if err := toolReg.Register(meta); err != nil {
			return apperr.Wrap(apperr.CodeInternal,
				fmt.Sprintf("builtin_tools: register %q", d.name), err)
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
