# ADR-0004: Tier-0 8GB 内存硬上限 + Hardware Tier 解锁机制

- **状态**: Accepted（回填）
- **日期**: 2026-05-16
- **决策者**: 架构组
- **相关模块**: 全系统级(HE-Rules 之上)
- **实现详情**: [ARCHITECTURE §2 + §4](../ARCHITECTURE.md) | [00-Dict §1 Tier-X-Limit](../00-Global-Dictionary.md) | [M03 §5 AutoConfig](../M03-Observability.md)

## 上下文

LLM 类应用倾向占满内存(embedding 模型/HNSW 索引/Wasm 运行时/缓存)。主流开发者笔记本配置 8/16/24/64GB。默认 16GB+ 排除入门用户,硬限 8GB 则丢强能力(gVisor / QLoRA / 大模型本地推理)。

## 决策

**Tier-0 8GB 是核心路径硬上限;超额能力通过 Hardware Tier 显式解锁,不作默认。**

四级 Tier(HT0/HT1/HT2/HT3 = 8/16/24/64GB)完整定义见 [ARCHITECTURE §2](../ARCHITECTURE.md)。

所有超额能力**必须**:
- 在 FeatureGate 后,HT0 默认关闭
- 在 `00-Global-Dictionary §1` 显式声明所需 Tier
- 提供 HT0 降级路径(功能弱化但不报错)

## 被驳与反例守护

| 方案 | 驳回理由 |
|------|---------|
| 4GB 上限 | 主流 LLM 推理 + embedding 模型加载困难;过度收紧 |
| 32GB 起点 | 排除大部分开发者笔记本;不适合个人 Agent 定位 |
| 无硬性上限 | 工程纪律失锚;很快滑向"得需 64GB"的不可持续 |
| 单一 Tier(不分级) | 强能力被一刀切,硬件富裕用户无法解锁全功能 |

**反例守护**:未来如有人提议"添加 X 功能但只能 32GB+ 运行"作为默认路径—本 ADR 拒绝。必须 FeatureGate 后绑 HT1+/HT2+。
