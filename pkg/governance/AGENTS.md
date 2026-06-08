# pkg/governance/ (L3 治理: M12 评测套件)

> Canonical arch doc: [M12-Eval-Harness.md](../../docs/arch/M12-Eval-Harness.md)

**硬约束**:
1. SSoT 对齐: Eval 任务/阈值/回归判定必与 `state.yaml` 严格一致 (含 spec_consistency_test)
2. 元递归边界: `--ci-gate` 仅评测系统行为, 禁评测 polaris 自身代码质量
3. 回归双阈: 偏离 RollingBaseline(24h) >2σ→WARN; >3σ→CRITICAL
4. 影子执行: ShadowExecutor 结果仅落 audit_log, 禁写回业务表
5. 依赖单向: 读 L0~L2 走 protocol; 禁向下层主动调用副作用

**高频陷阱**:
- 阈值变更必同步 ADR (解释原因)
- 检测滑窗不同: M12 CI 触发; M3 `[Window-Quality-10min]` 运行时滑窗
- ProgressiveRollout 评估只读 M3, 不写
- 根层 `eval.go` 已 legacy, 用 `eval/` 子包
- 评估结果发 EventBus, 订阅者自取

**文件索引**:
- [标杆] `eval/runner.go`: RunnerImpl (套件执行器)
- [参照] `eval/eval.go`: Evaluator 五级类型
- [参照] `eval/store.go`: SQLiteEvalStore
- [参照] `shadow_executor.go`: 影子对比执行
- [参照] `trajectory_recorder.go`: 零 LLM 重放
- [参照] `policy/engine.go`: 评估侧策略
- [参照] `synthetic/generator.go`: 合成测试数据生成

**跨模块**:
- 读 L0~L2 走 protocol; 禁向下层主动调用副作用
- 评估结果发 EventBus, 订阅者自取
