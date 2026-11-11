package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/polarisagi/polaris/internal/eval/harness"
	"github.com/polarisagi/polaris/internal/eval/harness/benchmark"
	"github.com/polarisagi/polaris/internal/protocol"
	"github.com/polarisagi/polaris/internal/tool/builtin/guard"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/types"
)

func runEvalBenchCmd(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	suite := fs.String("suite", "", "基准套件名称 (例如 tau-bench)")
	dataPath := fs.String("data", "", "本地数据集路径")
	projectRoot := fs.String("project-root", "", "可选：明确授权的本地项目根目录")
	outPath := fs.String("out", "", "可选：输出报告 JSON 路径")
	execute := fs.Bool("execute", false, "是否真实运行评测")

	if err := fs.Parse(args); err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "parse bench flags failed", err)
	}
	if *suite == "" || *dataPath == "" {
		return apperr.New(apperr.CodeInvalidInput, "用法: polaris eval bench --suite=<suite> --data=<path> [--project-root=<path>] [--execute] [--out=<report.json>]")
	}

	adapter := benchmark.GetAdapter(*suite)
	if adapter == nil {
		return apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("未知的基准套件: %s", *suite))
	}

	ctx := context.Background()
	if *projectRoot != "" {
		root, err := resolveEvalProjectRoot(*projectRoot)
		if err != nil {
			return err
		}
		ctx = protocol.WithProjectRoot(ctx, root)
		if err := validateDatasetInProject(*dataPath, root); err != nil {
			return err
		}
	}
	cases, err := adapter.Load(ctx, *dataPath)
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "load dataset failed", err)
	}

	fmt.Printf("加载了 %d 条来自 %s 的测试用例\n", len(cases), *suite)

	if *execute {
		// This should not happen because main.go intercepts --execute
		return apperr.New(apperr.CodeInternal, "--execute mode must be intercepted by main.go")
	}

	report := map[string]any{
		"suite":    *suite,
		"total":    len(cases),
		"executed": false,
		"note":     "本次调用仅验证数据集加载和转换，没有执行评测；使用 --execute 可运行评测",
	}

	if *outPath != "" {
		b, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return apperr.Wrap(apperr.CodeInternal, "failed to marshal report", err)
		}
		if err := os.WriteFile(*outPath, b, 0644); err != nil {
			return apperr.Wrap(apperr.CodeInternal, "failed to write report", err)
		}
		fmt.Printf("报告已写入 %s（仅数据集加载校验，未执行评测）\n", *outPath)
	} else {
		fmt.Printf("数据集加载校验完成: 总计 %d 条用例；本次未执行评测（使用 --execute 运行）\n", len(cases))
	}

	return nil
}

func runEvalBenchCmdWithDeps(args []string, store *harness.SQLiteEvalStore, runner *harness.RunnerImpl) error {
	options, err := parseExecuteEvalBenchOptions(args)
	if err != nil {
		return err
	}
	if runner == nil {
		return apperr.New(apperr.CodeServiceUnavailable, "eval benchmark runner is unavailable")
	}
	ctx, cases, err := loadEvalBenchmarkCases(options.suite, options.dataPath, options.projectRoot)
	if err != nil {
		return err
	}
	fmt.Printf("加载了 %d 条来自 %s 的测试用例\n", len(cases), options.suite)
	_ = store // benchmark 数据集不写入 M9 training/validation 分区。
	fmt.Printf("开始执行评测...\n")
	caseSet := benchmarkCasesAsAny(cases)
	report, err := runner.RunBenchmarkDataset(ctx, options.suite, caseSet, "cli")
	if err != nil {
		return apperr.Wrap(apperr.CodeOf(err), "runner failed", err)
	}
	if options.outPath != "" {
		if err := writeEvalBenchmarkReport(options.outPath, report); err != nil {
			return err
		}
		fmt.Printf("完整报告已写入 %s\n", options.outPath)
	}
	if report.Status != "completed" {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf("eval benchmark failed: pass=%d fail=%d", report.PassCount, report.FailCount))
	}
	fmt.Printf("评测执行完成: pass=%d fail=%d\n", report.PassCount, report.FailCount)
	return nil
}

type executeEvalBenchOptions struct {
	suite       string
	dataPath    string
	projectRoot string
	outPath     string
}

func parseExecuteEvalBenchOptions(args []string) (executeEvalBenchOptions, error) {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	suite := fs.String("suite", "", "基准套件名称 (例如 tau-bench)")
	dataPath := fs.String("data", "", "本地数据集路径")
	projectRoot := fs.String("project-root", "", "可选：明确授权的本地项目根目录")
	outPath := fs.String("out", "", "可选：输出报告 JSON 路径")
	execute := fs.Bool("execute", false, "是否真实运行评测")

	if err := fs.Parse(args); err != nil {
		return executeEvalBenchOptions{}, apperr.Wrap(apperr.CodeInvalidInput, "parse bench flags failed", err)
	}
	if *suite == "" || *dataPath == "" || !*execute {
		return executeEvalBenchOptions{}, apperr.New(apperr.CodeInvalidInput, "用法: polaris eval bench --suite=<suite> --data=<path> --execute [--project-root=<path>] [--out=<report.json>]")
	}
	return executeEvalBenchOptions{suite: *suite, dataPath: *dataPath, projectRoot: *projectRoot, outPath: *outPath}, nil
}

func loadEvalBenchmarkCases(suite, dataPath, projectRoot string) (context.Context, []protocol.EvalCase, error) {
	adapter := benchmark.GetAdapter(suite)
	if adapter == nil {
		return nil, nil, apperr.New(apperr.CodeInvalidInput, fmt.Sprintf("未知的基准套件: %s", suite))
	}
	ctx := context.Background()
	if projectRoot != "" {
		root, err := resolveEvalProjectRoot(projectRoot)
		if err != nil {
			return nil, nil, err
		}
		ctx = protocol.WithProjectRoot(ctx, root)
		if err := validateDatasetInProject(dataPath, root); err != nil {
			return nil, nil, err
		}
	}
	cases, err := adapter.Load(ctx, dataPath)
	if err != nil {
		return nil, nil, apperr.Wrap(apperr.CodeInternal, "load dataset failed", err)
	}
	return ctx, cases, nil
}

func benchmarkCasesAsAny(cases []protocol.EvalCase) []any {
	caseSet := make([]any, len(cases))
	for i := range cases {
		caseSet[i] = cases[i]
	}
	return caseSet
}

func writeEvalBenchmarkReport(path string, report *types.EvalRunReport) error {
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return apperr.Wrap(apperr.CodeInternal, "failed to marshal report", err)
	}
	if err := os.WriteFile(path, b, 0644); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "failed to write report", err)
	}
	return nil
}

func resolveEvalProjectRoot(path string) (string, error) {
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", apperr.Wrap(apperr.CodeInvalidInput, "eval benchmark: resolve project root", err)
		}
		path = abs
	}
	root, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", apperr.Wrap(apperr.CodeInvalidInput, "eval benchmark: project root unavailable", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", apperr.New(apperr.CodeInvalidInput, "eval benchmark: project root must be a local directory")
	}
	if err := guard.CheckScopeRoot(root); err != nil {
		return "", apperr.Wrap(apperr.CodeForbidden, "eval benchmark: unsafe project root", err)
	}
	return root, nil
}

func validateDatasetInProject(path, root string) error {
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "eval benchmark: project root unavailable", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "eval benchmark: resolve dataset path", err)
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return apperr.Wrap(apperr.CodeInvalidInput, "eval benchmark: dataset unavailable", err)
	}
	if err := guard.CheckReadablePath(context.Background(), canonical, []string{canonicalRoot}); err != nil {
		return apperr.Wrap(apperr.CodeForbidden, "eval benchmark: dataset must be inside the authorized project root", err)
	}
	return nil
}
