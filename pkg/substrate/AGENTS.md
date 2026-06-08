# pkg/substrate/ (L0 基础: M1 推理/M2 存储/M3 观测/M11 策略)

> Canonical arch docs: [M01-Inference-Runtime.md](../../docs/arch/M01-Inference-Runtime.md) · [M02-Storage-Fabric.md](../../docs/arch/M02-Storage-Fabric.md) · [M03-Observability.md](../../docs/arch/M03-Observability.md) · [M11-Policy-Safety.md](../../docs/arch/M11-Policy-Safety.md)

**硬约束**:
1. 依赖单向: 禁引用任何其它 pkg/, 仅可引 internal/
2. 单写者: 写入必走 MutationBus, 禁裸 SQL (XR-04); CAS 场景例外 (须注释说明)
3. 指标命名: `polaris_{subsystem}_{name}_{unit}`
4. FFI 隔离: 必经 policy.Gate; 共享 ffi loader (校验 ABI 1.0)
5. 安全出站: 所有 Dial/Get 走 policy.SafeDialer (XR-06)
6. append-only: events 表禁 UPDATE, 变更写 change_log

**高频陷阱**:
- 幂等键锁死: `{engine}:{type}:{id}:{op}:{version}`, 禁偷懒拼接
- TokenBurnRate 单源: M3 gauge 为唯一真相, M4/M11/M13 读取不重新采样
- 污点传播: TaintLevel 只升不降, 传播用 taint.Max
- Tier 0 内存: 与 ResourceGovernor 共享 L1~L3 降级阈值
- STT 本地推理: Sherpa-ONNX (sherpa.go), 仅 HT1+ 解锁, Tier 0 禁用

**文件索引**:
- [标杆] `storage/store.go`: SQLiteStore (WAL/串行写)
- [标杆] `storage/surreal_store.go`: SurrealDBCoreStore (FFI)
- [标杆] `observability/metrics.go`: 观测单例 (豁免 R1.3)
- [标杆] `policy/gate.go`: Cedar Three-Layer Gate
- [标杆] `inference/adapter_anthropic.go`: Provider 适配 canonical
- [参照] `storage/eventlog.go`: events 表追加写
- [参照] `storage/decisionlog.go`: 决策日志
- [参照] `policy/taint_gate.go`: 污点门控
- [参照] `policy/pii_detector.go`: PII 扫描
- [参照] `inference/router.go`: Provider 路由
- [参照] `inference/stt/sherpa.go`: 本地 STT (HT1+)
- [参照] `observability/feature_gate.go`: 硬件门控解锁
- [参照] `observability/auto_config.go`: 启动时硬件探测

**跨模块**:
- 暴露给上层仅经 `protocol/interfaces.go`
- 接口/符号变更走 B5 `[proto-break]`
