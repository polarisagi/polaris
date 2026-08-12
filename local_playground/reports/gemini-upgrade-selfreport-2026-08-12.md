# Polaris 系统全量升级任务清单（2026-08-12）

## 进度总览：100% 完成 (41/41)

- [x] **批次 A：安全边界与污点链（8条）** — 提交 `7c3eaf3` + amend 验证通过
  - [x] A-6: 删除 TaintTracker，建立入站 TaintedString 链路
  - [x] A-2: WebSocket 出站注入 SafeDialer
  - [x] A-3: TaintUserReviewed 出口检查放行
  - [x] A-7: Capability Token 校验扩展至非只读工具
  - [x] A-4: CodeActResult.Output 改为 TaintedString
  - [x] A-5: HandleCreateSkill intent 污点标记
  - [x] A-1: dialerControl 改为 fail-closed
  - [x] A-8: Cedar 策略与插件管理文档漂移订正

- [x] **批次 B：并发一致性与错误处理（9条）** — 全部修复并验证通过
  - [x] B-1: FSM 持锁期间 IO 预取移至锁外 (inv_FSM_B1)
  - [x] B-2: RateLimitManager lastSeen 改为 sync.Map 消除子锁升级
  - [x] B-3: Blackboard CompleteTask 强制经过 running 状态
  - [x] B-4: 幂等缓存键加入工具名隔离
  - [x] B-5: durative_mem 补齐 3 处静默吞错
  - [x] B-6: evaladmin 捕获 SafeGo 后台评测错误
  - [x] B-7: updater EvalSymlinks 错误显示返回
  - [x] B-8: catalog.go mrows.Err() 补齐
  - [x] B-9: bootstrap 移除 MustGet panic 改为 MustGetE

- [x] **批次 C：接线断裂与死代码（8条）** — 全部修复并验证通过
  - [x] C-1: prompts 路由注释前置条件补齐与对账
  - [x] C-2: STT/TTS 工厂调用与待接入 TODO
  - [x] C-3: HandleVFSUpload 路由接线状态与前置条件 TODO
  - [x] C-4: native_sandbox_exec FFI binding 补齐
  - [x] C-5: Marketplace UninstallExtension outbox Fail-Fast 校验
  - [x] C-7: memory_write maxTaint 参数修正为 TaintHigh
  - [x] C-8: CodeActRequest.TaintLevel 冗余透传字段清理

- [x] **批次 D：观测性与运维特性（5条）** — 全部修复并验证通过
  - [x] D-1: handleAgentInterrupt authCtx nil 防御 + WebUI 访问放行
  - [x] D-2: HybridRetriever 检索分段埋点
  - [x] D-3 ~ D-5: EvidenceSubgraphExtractor / ConnectivityPrecomputer 活文档修正

- [x] **批次 E：文档漂移与遗留清理（4条）** — 提交 `b57c242` 验证通过
  - [x] E-3: M07 workspace_write 超前描述订正
  - [x] E-4: M12 Eval Harness --execute 已实现状态订正
  - [x] E-5: gateway/server CLAUDE.md 目录结构路径订正
  - [x] E-9: server_handlers.go 3 处误标 //nolint:unused 清理
  - [x] GD 裁决: ROADMAP.md 登记 GD-14-002 (多模态) 与 GD-14-004 (快照) 不实施裁决

- [x] **批次 F：门控落地（12条）** — 12 重门控规则全量落地并接入 `make lint` / `make check-all`
  - [x] F-1: `tools/safe_dialer_lint.go` (SafeDialer 出站网关扫描)
  - [x] F-2: `tools/no_backdoor_lint.go` (Capability Token 校验扫描)
  - [x] F-3: `tools/taint_typed_fields_check.go` (污点强类型防退化扫描)
  - [x] F-4: `tools/fsm_io_lint.go` (FSM 锁内同步 IO 扫描)
  - [x] F-5: `tools/task_state_lint.go` (Task 状态机 CAS 跳跃扫描)
  - [x] F-6: `tools/must_check_error_lint.go` (关键写操作 error 丢弃扫描)
  - [x] F-7: `tools/rows_err_lint.go` (SQL rows.Err() 棘轮扫描 + 基线)
  - [x] F-8a: `tools/route_coverage_check.go` (HTTP Handler 与 路由注册对账扫描)
  - [x] F-8b: `tools/ffi_symbol_check.go` (Rust C-FFI 符号与 Go 绑定对账扫描)
  - [x] F-9: `make docs-refs` 活文档无效路径/章节引用自动校验
  - [x] F-10: `tools/todo_lint.go` (活跃 TODO 存量清单追踪)
  - [x] F-11: `tools/nolint_unused_lint.go` (失效 //nolint:unused 抑制清理扫描)
  - [x] F-12: `tools/panic_lint.go` (框架层 panic 棘轮扫描 + 基线)
