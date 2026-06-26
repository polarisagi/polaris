// boot_server.go — §11~§11.5 启动阶段：
// HTTP Server 装配 → LogicCollapseMonitor（P2-FIX）→ OTA 热更新管理器 → STT/TTS → Start。
// 返回 *server.Server 供 run() 执行 Shutdown 和 printStartupSummary。
package main

import (
	"github.com/polarisagi/polaris/internal/llm"
	"github.com/polarisagi/polaris/internal/observability/probe"

	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/polarisagi/polaris/internal/prompt/optimizer"

	"golang.org/x/time/rate"

	"github.com/polarisagi/polaris/internal/action/codeact"
	extskill "github.com/polarisagi/polaris/internal/extension/skill"
	"github.com/polarisagi/polaris/internal/gateway/server"
	"github.com/polarisagi/polaris/internal/gateway/server/plugin"
	si "github.com/polarisagi/polaris/internal/learning"
	"github.com/polarisagi/polaris/internal/sandbox"
	swarmAgents "github.com/polarisagi/polaris/internal/swarm/agents"
	"github.com/polarisagi/polaris/internal/sysmgr/updater"
	"github.com/polarisagi/polaris/pkg/types"
)

// bootServer 执行 §11~§11.5 初始化：装配 HTTP Server、OTA 管理器、STT/TTS，并调用 Start()。
// 返回 *server.Server，调用方 run() 负责 Shutdown。
func bootServer(ctx context.Context, sb *SubstrateBundle, tb *ToolBundle, ab *AgentBundle) (*server.Server, error) { //nolint:gocyclo
	addr := fmt.Sprintf("%s:%d", sb.Cfg.Interface.Host, sb.Cfg.Interface.Port)
	apiRateLimiter := rate.NewLimiter(rate.Limit(50), 100)
	httpServer := server.NewServer(addr, sb.DataDir, ab.AgentPool, ab.Blackboard, tb.HITLGateway,
		sb.Store.DB(), sb.InfReg, sb.SafeHTTP, sb.Dialer, sb.Cfg.Compressor, sb.TBR, apiRateLimiter)
	httpServer.SetAuditTrail(sb.AuditTrail)
	httpServer.SetLogStore(sb.LogStore)
	httpServer.SetToolRegistry(tb.ToolReg)
	httpServer.SetSkillRegistry(tb.SkillRegistry)
	httpServer.SetEmbedder(sb.Embedder, sb.Cfg.Embedding.Threshold)

	// 设置插件同步向量索引器
	if sb.SurrealStore != nil {
		idx := plugin.NewEmbeddingIndexer(&pluginCognIndexAdapter{s: sb.SurrealStore}, sb.Embedder)
		httpServer.SetEmbeddingIndexer(idx)
	}

	// ─── Skill 签名密钥 ──────────────────────────────────────────────────────
	var skillSigningKey []byte
	if key := os.Getenv("POLARIS_SKILL_SIGNING_KEY"); key != "" { //nolint:nestif
		skillSigningKey = []byte(key)
	} else {
		if b, err := os.ReadFile(sb.Layout.SkillSignKey); err == nil && len(b) > 0 {
			skillSigningKey = b
		} else {
			h := sha256.Sum256(fmt.Appendf(nil, "polaris-local-%d", time.Now().UnixNano()))
			skillSigningKey = h[:]
			if err := os.WriteFile(sb.Layout.SkillSignKey, skillSigningKey, 0600); err != nil {
				slog.Warn("polaris: failed to write skill_signing.key", "err", err)
			}
		}
	}

	// ─── [B4-F2] CodeAct 引擎初始化（FeatureL3Sandbox 门控）───────────────────
	// CodeAct 强制依赖 Sbx-L3（ContainerSandbox）；L3 未启用时跳过。
	// 架构文档: docs/arch/M07-Tool-Action-Layer.md §7.4
	if tb.ContainerSandbox != nil {
		// GovernanceAgent 是 CodeAct 的 L1 代码安全校验网关（必须非 nil）。
		// policyGate：protocol.PolicyGate 接口；security/policy.Gate 通过 IsAuthorized 满足。
		govAgent, _ := swarmAgents.NewGovernanceAgent(sb.Gate, sb.Store.DB())
		codeActEngine := codeact.NewCodeAct(
			tb.Envelope,
			nil, // toolExec 审计可选；nil 时跳过 RecordAudit 调用
			codeact.WithGovernanceAgent(&govAgentAdapter{inner: govAgent}),
			codeact.WithHITL(tb.HITLGateway),
		)
		ab.Agent.SetCodeAct(codeActEngine)
		httpServer.SetCodeActEngine(codeActEngine)
		slog.Info("polaris: CodeAct engine initialized and injected",
			"sandbox", "L3-container",
			"backend", sb.AutoConf.Config.L3SandboxBackend,
		)
	} else {
		slog.Info("polaris: CodeAct engine skipped (FeatureL3Sandbox disabled or ContainerSandbox nil)")
	}
	// ─── [/B4-F2] ────────────────────────────────────────────────────────────

	// ─── [P2-FIX] M9：LogicCollapseMonitor + StagingPipeline ────────────────
	// 在 skillSigningKey 初始化之后创建，确保编译器签名密钥可用。
	// FeatureLogicCollapse 门控：内存 < 1024MB 时跳过（避免在 Tier-0 低内存 VPS 加重负担）。
	logicCollapseEnabled := sb.AutoConf != nil &&
		sb.AutoConf.Gate.State(probe.FeatureLogicCollapse) != probe.FeatureDisabled

	if logicCollapseEnabled && tb.ContainerSandbox != nil {
		collapseCompilerTier := sb.AutoConf.Config.Tier
		collapseCompiler := extskill.NewLogicCollapseCompiler(extskill.LogicCollapseConfig{
			Gate:             extskill.NewCompileGate(collapseCompilerTier),
			CodeGen:          si.NewDefaultLLMCodeGenerator(sb.Router),
			Registry:         tb.SkillRegistry,
			WorkDir:          sb.Layout.Workspace,
			SigningKey:       skillSigningKey,
			ContainerSandbox: tb.ContainerSandbox,
		})
		collapseMonitor := si.NewLogicCollapseMonitor(
			collapseCompiler,
			si.NewDefaultLLMCodeGenerator(sb.Router),
			tb.SkillRegistry,
			&hitlNotifierAdapter{gateway: tb.HITLGateway},
			skillSigningKey,
			sb.Layout.Workspace,
		)
		rolloutStore, rolloutStoreErr := optimizer.NewSQLiteRolloutStore(sb.Store.DB())
		if rolloutStoreErr != nil {
			slog.Warn("polaris: failed to init SQLiteRolloutStore, L2 staging disabled", "err", rolloutStoreErr)
		} else {
			collapseMonitor.WithStagingPipeline(rolloutStore)
			slog.Info("polaris: StagingPipeline injected into LogicCollapseMonitor")
		}
		ab.Agent.SetToolCallRecorder(&collapseRecorderAdapter{m: collapseMonitor})
		slog.Info("polaris: LogicCollapseMonitor injected as ToolCallRecorder into agent kernel",
			"feature", probe.FeatureLogicCollapse,
			"tier", collapseCompilerTier,
		)
	} else {
		slog.Info("polaris: LogicCollapseMonitor skipped (FeatureLogicCollapse disabled, AutoConf nil, or ContainerSandbox nil)")
	}
	// ─── [/P2-FIX] ───────────────────────────────────────────────────────────

	// ─── OTA 热更新管理器 ─────────────────────────────────────────────────────
	updMgr := updater.New(Version, CommitHash, BuildDate, sb.SafeHTTP)
	updMgr.SetRestartFn(func() {
		// syscall.Exec 完全替换进程镜像，Go runtime 不执行任何 defer/finalizer。
		// 关闭序列（与 graceful shutdown 保持一致）：
		//   1. stop()         — 取消主 ctx，dbWriter.Run() flush 残余批次并退出
		//   2. <-dbWriterDone — 确认 DatabaseWriter goroutine 完全退出
		//   3. store.Close()  — SQLite WAL checkpoint + 清理 .db-wal / .db-shm
		exe, _ := os.Executable()
		slog.Info("polaris: initiating graceful db shutdown before exec-restart")
		sb.Stop()
		select {
		case <-sb.DBWriterDone:
		case <-time.After(5 * time.Second):
			slog.Warn("polaris: dbWriter flush timeout during hot-restart, proceeding anyway")
		}
		_ = sb.Store.Close()
		slog.Info("polaris: exec-restarting with new binary", "path", exe)
		_ = syscall.Exec(exe, os.Args, os.Environ())
		os.Exit(0)
	})
	httpServer.SetUpdater(updMgr)

	// ─── 其余 Server 装配 ─────────────────────────────────────────────────────
	httpServer.SetInstallManager(tb.InstallMgr)
	httpServer.SetSkillSigningKey(skillSigningKey)
	httpServer.SetMCPManager(tb.MCPMgr)
	if tb.ContainerSandbox != nil {
		httpServer.SetScriptRunner(tb.ContainerSandbox)
	}
	httpServer.SetToolRegistry(tb.ToolReg)
	httpServer.SetSkillRegistry(tb.SkillRegistry)
	httpServer.SetToolExecutor(func(ctx context.Context, name string, args []byte) (*types.ToolResult, error) {
		// script runtime 技能：LLM 工具名格式 "skill__{slug}"，内部 DB 名为 "skill:{slug}"
		if slug, ok := strings.CutPrefix(name, "skill__"); ok {
			var instructions string
			_ = sb.Store.DB().QueryRowContext(ctx,
				`SELECT instructions FROM skills WHERE name=? AND deprecated=0`, "skill:"+slug).Scan(&instructions)
			var req struct {
				Input string `json:"input"`
			}
			_ = json.Unmarshal(args, &req)
			output := instructions
			if req.Input != "" {
				output += "\n\n---\n\n输入：" + req.Input
			}
			return &types.ToolResult{Output: []byte(output)}, nil
		}
		res, err := tb.Envelope.Execute(ctx, sandbox.ExecRequest{
			Principal: sandbox.PrincipalAgent, Kind: sandbox.KindToolExecute,
			Resource: name, TrustTier: types.TrustUntrusted, Tool: types.Tool{Name: name},
			Input: args, TaintLevel: types.TaintNone,
		})
		if err != nil {
			return nil, err
		}
		return &types.ToolResult{Success: res.Success, Output: res.Output, Error: res.Error,
			LatencyMs: res.LatencyMs, TaintLevel: res.TaintLevel, ImageParts: res.ImageParts}, nil
	})
	httpServer.SetLogStore(sb.LogStore)
	httpServer.SetEvalRunner(ab.EvalRunner)

	// ─── §11.5 STT/TTS 引擎初始化（FeatureLocalSTT 门控，异步下载，不阻塞启动）
	var sttGate *probe.FeatureGate
	if sb.AutoConf != nil {
		sttGate = sb.AutoConf.Gate
	}
	httpServer.InitSTTEngine(ctx, sb.DataDir, sttGate, sb.SafeHTTP, sb.Cfg.Inference.STT)
	httpServer.InitTTSEngine(ctx, sb.DataDir, sttGate, sb.SafeHTTP, sb.Cfg.Inference.TTS)

	// ─── §11.6 后台向量回填触发器 (Dynamic Embedding Backfill)
	if dyn, ok := sb.Embedder.(*llm.DynamicEmbedder); ok {
		go func() {
			<-dyn.WaitReady()
			slog.Info("polaris: Dynamic Embedder ready, triggering background plugin vector backfill...")
			if httpServer.PluginHandler() != nil {
				_, err := httpServer.PluginHandler().SyncAllMarketplaces(context.Background(), true)
				if err != nil {
					slog.Warn("polaris: Background vector backfill encountered errors", "err", err)
				}
			}
		}()
	}

	if err := httpServer.Start(); err != nil {
		slog.Error("polaris: failed to start HTTP server", "err", err)
		return nil, err
	}

	return httpServer, nil
}
