# ADR-0004: Tier-0 8GB 内存硬上限 + Hardware Tier 解锁机制

- **状态**: Accepted | **日期**: 2026-05-16 | **模块**: 全系统级
- **实现详情**: [ARCHITECTURE §2+§4](../ARCHITECTURE.md) | [00-Dict §1 Tier-X-Limit](../00-Global-Dictionary.md) | [M03 §5 AutoConfig](../M03-Observability.md)

## 决策

Tier-0（8GB）是核心路径硬上限；超额能力通过四级 Hardware Tier（HT0/HT1/HT2/HT3 = 8/16/24/64GB，定义见 ARCHITECTURE §2）显式解锁，不作默认。

所有超额能力必须：在 FeatureGate 后、HT0 默认关闭；在 `00-Global-Dictionary §1` 显式声明所需 Tier；提供 HT0 降级路径（弱化不报错）。

## 反例守护

拒绝"新功能默认只能 32GB+ 运行"——必须 FeatureGate 后绑 HT1+/HT2+。拒绝无硬性上限（工程纪律失锚）与单一不分级 Tier（强能力无法按硬件解锁）。

## 引用代码

`internal/observability/probe/{hardware_probe,feature_gate,tier_parameters,memory_probe}.go`
