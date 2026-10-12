# ADR-0004: Tier-0 内存地板 + Hardware Tier 解锁机制

> 状态：Accepted（2026-10 口径修订，见沿革）｜ 首次：2026-05-16 ｜ 最近复核：2026-10-12 ｜ 模块：全系统
> 实现详情：[ARCHITECTURE §2+§4](../ARCHITECTURE.md)、[00-Dict §1 Tier-X-Limit](../00-Global-Dictionary.md)、[M03 §5 AutoConfig](../M03-Observability.md)

## 决策一：核心路径预算有硬上限，超额能力按 Hardware Tier 解锁

现行规则（CLAUDE.md §不变量 [Tier-0]）：
- 核心路径（SurrealDB kv-mem + Embedding + Wasm 沙箱）必须在 **2GB+ VPS** 可运行；语音按三档（A <1800MB 或 <2 核全关 / B <3600MB 只 STT / C ≥3600MB 且 ≥2 核 STT+服务端 TTS），见 [ADR-0110](./ADR-0110-audio-stt-tts-hardware-tiers.md)。
- **8GB 是开发推荐地板（HT0）**，不再是"硬上限"；本地推理需 HT1（16GB+）。
- 四级 Hardware Tier：HT0/HT1/HT2/HT3 = 8/16/24/64GB（`hardware_probe.go computeTier`：<8GB 仍归 Tier0 并标 degraded）。

超额能力必须：
- 置于 FeatureGate 后，HT0 默认关闭
- 在 `00-Global-Dictionary §1` 声明所需 Tier
- 提供 HT0 降级路径（弱化不报错）

## 反例守护 / 已驳方案

- "新功能默认只能 32GB+ 运行" — 必须 FeatureGate 后绑 HT1+/HT2+
- 无硬性上限 — 工程纪律失锚
- 单一不分级 Tier — 强能力无法按硬件解锁

## 重新评估触发条件

核心路径在实测中被证明无法在 2GB VPS 稳定运行且无可行优化时，才重议上调核心路径下限；"新功能想要更多内存"不构成触发，须先走 FeatureGate + Tier 声明。

## 沿革

| 日期 | 变更 | 出处 |
|---|---|---|
| 2026-05-16 | 初版：Tier-0 = 8GB 核心路径硬上限 | 原决策 |
| 2026-10-02 | 产品定位调整：核心路径最低 2GB VPS，8GB 降为开发推荐地板 | ADR-0107 定位变更（并入 ADR-0110）、CLAUDE.md §不变量 [Tier-0] |

## 引用代码

`internal/observability/probe/{hardware_probe,feature_gate,tier_parameters,memory_probe}.go`
