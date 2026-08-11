package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/polarisagi/polaris/pkg/apperr"
)

// `polaris eval --ci-gate` —— HE-4 的 CI 门控入口（M12 §4 Eval Runner「CI 门控」）。
//
// 注：迁出前 main.go 里的注释写的是「§10.8 Eval Harness CI Gate」，而 M12 §10 是
// 「增量快照」，该章节号早已失真（`make docs-refs` 的 anchor-refs 项抓出）。
//
// # 本门控不需要 LLM API Key
//
// 2026-08-11 复核结论：L1~L3 用例全程离线；只有 Level4LLMJudge 需要 Provider，
// 而无 Provider 时 runner 已改为 fail-safe（P0 判失败、非 P0 计入 L4Unjudged），
// 不再静默按通过计。因此 CI 无需配置 DEEPSEEK_API_KEY / OPENAI_API_KEY——
// 强行要求 Key 只会把"本可离线运行的门控"挡在门外，反而更接近 HE-4 要拦的行为。
//
// # 为什么要单独一个函数而不是内联在 main.go
//
// 因为门控的**输出**和它的判定同等重要。原实现只在失败时打 pass/fail/safety_fail
// 三个数，成功时打一句 "eval ci-gate passed" + pass_count。那句话在 0 用例时同样
// 会打印——一个什么都没校验的门控，输出与真正跑过全套用例的门控一模一样。
// 这正是本仓库反复吃亏的 ADR-0091 模式（门控失效与门控通过长得一样）。
func runEvalCIGate(ctx context.Context, ab *AgentBundle) error {
	slog.Info("polaris: running eval --ci-gate validation suite")

	report, runErr := ab.EvalRunner.RunSuite(ctx, "validation", "ci")
	if runErr != nil {
		return apperr.Wrap(apperr.CodeInternal, "eval ci-gate execution failed", runErr)
	}

	// 全量指标一次性打出：任何一个数字异常都能在 CI 日志里直接看到，
	// 不必回头加日志重跑。
	slog.Info("polaris: eval ci-gate report",
		"total_cases", report.TotalCases,
		"pass", report.PassCount,
		"fail", report.FailCount,
		"p0_count", report.P0Count,
		"p0_fail", report.P0Fail,
		"p1_fail", report.P1Fail,
		"safety_fail", report.SafetyFail,
		"l4_unjudged", report.L4Unjudged,
		"skipped_low_falsifiability", report.SkippedLowFalsifiability,
	)

	if report.Status == "failed" {
		return apperr.New(apperr.CodeInternal, fmt.Sprintf(
			"eval ci-gate FAILED: total=%d pass=%d fail=%d p0_fail=%d safety_fail=%d",
			report.TotalCases, report.PassCount, report.FailCount, report.P0Fail, report.SafetyFail,
		))
	}

	// ── 0 用例：门控未配置，不是"通过" ──────────────────────────────────────
	//
	// validation 用例存在 eval store（SQLite）里，CI 每次都是全新临时库，因此
	// 目前恒为 0 条。旧实现此时打印 "eval ci-gate passed"，读日志的人会以为
	// 防退化门控在保护自己，实际上它一条都没校验。
	//
	// 刻意**不** exit 1：CI 天天红等于训练所有人忽略 CI，比静默通过更糟，而且
	// 让流水线红着也不会凭空长出用例来。取"绝不谎报 + 高可见度"：
	// 措辞明确写"未校验任何内容"，并在 GitHub 上打 ::warning:: 注解。
	if report.TotalCases == 0 {
		slog.Warn("polaris: eval ci-gate 未校验任何内容 —— validation 用例集为空（门控尚未配置用例）")
		ghAnnotate("warning", "Eval 门控未校验任何内容：validation 用例集为空。"+
			"本次 CI 的绿灯不代表通过了防退化校验（HE-4）。种子用例进 eval store 后本门控才真正生效。")
		return nil
	}

	// 有用例但全是"未评判"：同样不能当通过报。
	if report.L4Unjudged > 0 {
		slog.Warn("polaris: eval ci-gate 有用例未完成语义评判",
			"l4_unjudged", report.L4Unjudged, "total_cases", report.TotalCases)
		ghAnnotate("warning", fmt.Sprintf(
			"%d/%d 条用例未完成 L4 语义评判（无 Provider 或 Judge 调用失败）。"+
				"P0 用例已按失败处置；非 P0 的结论来自字符串检查而非语义评判。",
			report.L4Unjudged, report.TotalCases))
	}

	slog.Info("polaris: eval ci-gate passed",
		"total_cases", report.TotalCases, "pass", report.PassCount)
	return nil
}

// ghAnnotate 输出 GitHub Actions 注解（非 CI 环境下退化为普通 stderr 行）。
//
// 走 stderr 而非 slog：slog 的输出带时间戳与 level 前缀，GitHub 不会把它识别成
// 注解命令，那样这条提示就只是日志正文里的一行，而注解会出现在 run 摘要页顶部
// ——本函数存在的全部意义就是让"门控没校验任何东西"这件事出现在没人会去翻日志
// 的地方。
func ghAnnotate(level, msg string) {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		return
	}
	fmt.Fprintf(os.Stderr, "::%s title=Eval 门控::%s\n", level, msg)
}
