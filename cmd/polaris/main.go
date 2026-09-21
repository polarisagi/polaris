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
	"github.com/polarisagi/polaris/internal/security"
	"github.com/polarisagi/polaris/pkg/apperr"
	"github.com/polarisagi/polaris/pkg/concurrent"
)

func main() {
	if err := run(); err != nil {
		slog.Error("polaris: fatal", "err", err)
		os.Exit(1)
	}
}

func run() error { //nolint:gocyclo
	// ─── 0. 子命令分发 ──────────────────────────────────────────────────────
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve":
			// 显式启动守护进程：不在此返回，落到 switch 之外走完整 boot 流程。
			// 无参数启动等价于本命令，保留是为了让 service 单元文件与文档里的
			// 命令行自解释（"polaris" 单独一个词看不出它是在起服务还是在等输入）。
		case "service":
			return runServiceCmd(os.Args[2:])
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
		case "vault":
			return runVaultCmd(os.Args[2:])
		case "csv-fanout":
			return runCSVFanoutCmd(os.Args[2:])
		case "allowlist":
			return runAllowlistCmd(os.Args[2:])
		case "release-key":
			return runReleaseKeyCmd(os.Args[2:])
		case "unseal":
			return runUnsealCmd(os.Args[2:])
		case "seal-status":
			return runSealStatusCmd()
		case "skill":
			return runSkillCmd(os.Args[2:])
		case "eval":
			// "polaris eval --ci-gate" 是既有的 §10.8 CI 门禁入口，需要完整启动序列
			// （真实 EvalRunner，而非本 CLI 子命令组的纯 HTTP 客户端 runEvalCmd），
			// 不在此拦截，落到 switch 之外走下方完整 boot 流程；其余 polaris eval
			// 子命令（genkey/sign/meta-holdout/meta-audit）才归 runEvalCmd 处理。
			// GD-14-007: `eval bench --execute` 同样需要落入下方 boot 流程获取完整环境
			isBenchExecute := len(os.Args) >= 4 && os.Args[2] == "bench" && hasExecuteFlag(os.Args[3:])
			if len(os.Args) <= 2 || (os.Args[2] != "--ci-gate" && !isBenchExecute) {
				return runEvalCmd(os.Args[2:])
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
	concurrent.SafeGo(ctx, "main.sigint_watcher", func(ctx context.Context) {
		for range sigintCh {
			if ks != nil {
				ks.OnSIGINT()
			}
		}
	})

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

	// ─── §4.05 单实例锁 + 本地令牌（ADR-0096 决策五/六）────────────────────
	// 位置紧跟 bootSubstrate：它之后的每一步都在改写共享数据目录，两个实例并发
	// 走到那里会互相覆盖数据库与配置，现象却只是零星数据错乱，不指向"跑了两份"。
	rt, err := acquireRuntime(sb.Layout)
	if err != nil {
		return err
	}
	defer rt.release()

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
	// D4/ADR-0008：L4 长驻会话池持有子进程（Python/Bash 解释器），优雅关闭时
	// 必须显式终止，否则会成为孤儿进程。Shutdown() 对 nil 接收者安全
	// （未开启 sandbox.l4_enabled 时 tb.PersistentSandbox 为 nil）。
	defer tb.PersistentSandbox.Shutdown()

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
	if err := LoadProvidersFromDB(ctx, sb.Store.DB(), sb.Vault, sb.InfReg, sb.SafeHTTP, sb.TBR); err != nil {
		return apperr.Wrap(apperr.CodeInternal, "polaris-server: load providers from db failed", err)
	}

	// ─── §10.75 M04 §8 崩溃恢复回放 ──────────────────────────────────────────
	// 必须在此处（Provider 已就绪、HTTP 服务尚未开始对外服务）串行执行完毕：
	// 全局 protocol.ReplayMode 标志是进程级而非会话级，此窗口内不存在其他
	// 并发会话与其读取冲突（见 boot_crash_recovery.go 文件头注释）。
	recoverCrashedSessions(ctx, sb, ab)
	recoverAwaitingHandoffs(ctx, sb, ab)

	// ─── §10.8 Eval Harness CI Gate ─────────────────────────────────────────
	if len(os.Args) > 2 && os.Args[1] == "eval" {
		switch os.Args[2] {
		case "--ci-gate":
			return runEvalCIGate(ctx, ab)
		case "bench":
			return runEvalBenchCmdWithDeps(os.Args[3:], ab.EvalStore, ab.EvalRunner)
		}
	}

	// ─── §11 M13 Interface Server ────────────────────────────────────────────
	httpSrv, err := bootServer(ctx, sb, mb, tb, ab, rt.Token)
	if err != nil {
		return err
	}
	concurrent.SafeGo(ctx, "main.mcp_restore_servers", func(ctx context.Context) {
		tb.MCPMgr.RestoreServersFromDB(ctx, tb.ExtRepo, sb.DataDir)
	})

	// ─── §11.9 发布运行时状态 ───────────────────────────────────────────────
	// 端口取**实际绑定值**（配置为 0 时由内核分配）。写失败不阻断服务：服务本身
	// 是好的，只是本机客户端要靠 POLARIS_SERVER_URL 手动指定——但必须留痕，
	// 否则表现为"CLI 连不上一个正在运行的服务"且无任何线索。
	if port, perr := httpSrv.BoundPort(); perr != nil {
		slog.Warn("polaris: 未能取得实际绑定端口，运行时状态未发布", "err", perr)
	} else if perr = rt.publish(port); perr != nil {
		slog.Warn("polaris: 运行时状态发布失败，本机客户端需手动指定地址", "err", perr, "port", port)
	} else {
		slog.Info("polaris: 运行时状态已发布", "port", port, "run_dir", sb.Layout.Run)
	}

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
			if err := runInit(); err != nil {
				slog.Warn("polaris: 初始化向导未完成", "err", err)
			}
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
	// 30s 内没排空会返回 DeadlineExceeded——此时仍有在途请求被强行切断，
	// 与"干净停机"是两回事，不该看起来一样。
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("polaris: HTTP 服务未在超时内排空，存在被切断的在途请求", "err", err)
	}
	// 停机顺序 = 生产者先停、单写者后停（GR-3-001）：Supervisor workers 与 Reaper 都会写库，
	// 若留给 defer（LIFO 晚于本函数体）执行，它们会在 DBWriter.Close 之后继续提交。
	// 两者 Stop 均幂等，上方 defer 保留作异常返回路径兜底。
	ab.Supervisor.Stop()
	ab.ReaperStop()
	sb.EmbedBatcher.Stop()

	// 单写者：停止接收 → 排空残余 → 最终落盘；超时则放弃等待（进程即将退出）。
	concurrent.SafeGo(context.Background(), "polaris.shutdown.dbwriter_close", func(context.Context) { sb.DBWriter.Close() })
	select {
	case <-sb.DBWriterDone:
	case <-shutdownCtx.Done():
		slog.Warn("polaris: database writer flush timeout during shutdown")
	}

	// 审计链校验放在最终落盘之后，才能覆盖停机窗口内写入的事件。
	if rep, err := sb.AuditChain.VerifyChain(shutdownCtx, 0); err != nil {
		slog.Error("audit: chain verify failed on shutdown", "err", err)
	} else if !rep.Valid {
		slog.Error("audit: chain integrity broken", "report", rep)
	}

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

func hasExecuteFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--execute" {
			return true
		}
	}
	return false
}
