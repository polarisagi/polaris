// polaris main entry point.
// 启动序列由 boot_substrate → boot_memory → boot_tools → boot_knowledge → boot_agent → boot_server 组成。
// 架构文档: docs/arch/ARCHITECTURE.md §3 启动顺序
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/polarisagi/polaris/internal/config"
	"github.com/polarisagi/polaris/internal/gateway/server/provider"
	"github.com/polarisagi/polaris/internal/security"
	"github.com/polarisagi/polaris/pkg/apperr"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "polaris: fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error { //nolint:gocyclo
	// ─── 0. 子命令分发 ──────────────────────────────────────────────────────
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "init", "setup":
			return runInit()
		case "chat":
			return runChatCmd(os.Args[2:])
		case "status":
			return runCLIStatus()
		case "export":
			return runExport(os.Args[2:])
		case "import":
			return runImport(os.Args[2:])
		case "config":
			return runConfigCmd(os.Args[2:])
		case "version", "--version", "-v":
			fmt.Printf("polaris v%s\n", cliVersion())
			return nil
		case "help", "--help", "-h":
			printCLIHelp()
			return nil
		case "benchmark-routing":
			return runBenchmarkRouting(os.Args[2:])
		case "migrate":
			if len(os.Args) > 2 && os.Args[2] == "openclaw" {
				return runMigrateOpenClaw(os.Args[3:])
			}
		case "memory":
			if len(os.Args) > 2 && os.Args[2] == "process-staging" {
				return runProcessStaging()
			}
		}
	}

	// ─── 0. 信号监听 ────────────────────────────────────────────────────────
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// KillSwitch TripleCtrlCGuard（nil 安全，bootSubstrate 完成后赋值）
	var ks *security.KillSwitch
	sigintCh := make(chan os.Signal, 8)
	signal.Notify(sigintCh, syscall.SIGINT)
	go func() {
		for range sigintCh {
			if ks != nil {
				ks.OnSIGINT()
			}
		}
	}()

	// ─── §0.5~§4 L0 基础设施 ────────────────────────────────────────────────
	sb, err := bootSubstrate(ctx, stop)
	if err != nil {
		return err
	}
	if sb.LogFile != nil {
		defer sb.LogFile.Close()
	}
	defer sb.Store.Close()
	ks = sb.KS // TripleCtrlCGuard goroutine 现在可安全引用 ks

	// ─── §4.10~§5 记忆系统 + MEMF ──────────────────────────────────────────
	mb, err := bootMemory(ctx, sb)
	if err != nil {
		return err
	}

	// ─── §6~§6.8 工具层 ─────────────────────────────────────────────────────
	tb, err := bootTools(ctx, sb, mb)
	if err != nil {
		return err
	}

	// ─── §7~§7.7 知识 RAG ───────────────────────────────────────────────────
	kb, err := bootKnowledge(ctx, sb, mb, tb)
	if err != nil {
		return err
	}

	// ─── §8~§10.5 Agent Kernel + M9 + Supervisor ────────────────────────────
	ab, err := bootAgent(ctx, sb, mb, tb, kb)
	if err != nil {
		return err
	}
	// LIFO：Supervisor.Stop() 先于 ReaperStop() 执行（与原 defer 顺序一致）
	defer ab.ReaperStop()
	defer ab.Supervisor.Stop()
	// Supervisor workers 已注册，defers 已就位，现在安全启动
	ab.Supervisor.Start()
	slog.Info("polaris: supervisor tree started", "workers", 3)

	// ─── §10.7 从 DB 加载全部厂商配置（唯一合法的 Provider 注册路径）──────
	slog.Info("polaris-server: loading providers from db...")
	if err := provider.LoadProvidersFromDB(ctx, sb.Store.DB(), sb.InfReg, sb.SafeHTTP, sb.TBR); err != nil {
		slog.Error("polaris-server: failed to load providers from db", "error", err)
	}

	// ─── §10.8 Eval Harness CI Gate ─────────────────────────────────────────
	if len(os.Args) > 2 && os.Args[1] == "eval" && os.Args[2] == "--ci-gate" {
		slog.Info("polaris: running eval --ci-gate validation suite")
		report, runErr := ab.EvalRunner.RunSuite(ctx, "validation", "ci")
		if runErr != nil {
			return apperr.Wrap(apperr.CodeInternal, "eval ci-gate execution failed", runErr)
		}
		if report.Status == "failed" {
			return apperr.New(apperr.CodeInternal, fmt.Sprintf(
				"eval ci-gate failed: pass=%d fail=%d safety_fail=%d",
				report.PassCount, report.FailCount, report.SafetyFail,
			))
		}
		slog.Info("polaris: eval ci-gate passed", "pass_count", report.PassCount)
		return nil
	}

	// ─── §11 M13 Interface Server ────────────────────────────────────────────
	httpSrv, err := bootServer(ctx, sb, tb, ab)
	if err != nil {
		return err
	}
	go tb.MCPMgr.LoadFromDB(ctx, tb.ExtRepo, sb.DataDir)

	// ─── §12 启动摘要 ────────────────────────────────────────────────────────
	printStartupSummary(sb.Cfg, sb.Gate, sb.Router, mb.Mem, kb.Ingester, kb.Retriever,
		ab.EvalRunner, ab.Blackboard, ab.Sched, tb.HITLGateway, ab.Agent, ab.DAGExec, httpSrv)

	// ─── §13 零 Provider 引导（Zero-Provider Detection）─────────────────────
	var providerCount int
	if err := sb.Store.DB().QueryRow("SELECT COUNT(*) FROM providers").Scan(&providerCount); err != nil {
		slog.Warn("polaris: failed to check provider count from db", "err", err)
	}
	if providerCount == 0 {
		if cliTTY {
			_ = runInit()
		} else {
			slog.Warn("polaris: [Zero-Provider] No AI providers found in the database.")
			slog.Warn("polaris: Please visit http://localhost:28888 or run `polaris init` to configure the system.")
		}
	}

	// ─── §14 等待终止信号（优雅退出）────────────────────────────────────────
	slog.Info("polaris: system ready — waiting for signals (SIGINT/SIGTERM to exit)")
	<-ctx.Done()

	slog.Info("polaris: shutdown initiated, draining...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	ab.ReaperStop() // 显式提前停止 Reaper，确保在 dbWriter 排空前释放

	select {
	case <-sb.DBWriterDone:
	case <-shutdownCtx.Done():
		slog.Warn("polaris: database writer flush timeout during shutdown")
	}
	sb.DBWriter.Close()

	slog.Info("polaris: shutdown complete")
	return nil
}

// printStartupSummary 打印系统就绪摘要（components 为任意子系统实例，仅计数）。
func printStartupSummary(cfg *config.Config, components ...any) {
	slog.Info("polaris: system initialized",
		"tier", cfg.System.Tier,
		"max_agents", cfg.System.MaxAgents,
		"os", runtime.GOOS,
		"components", len(components),
	)
}
