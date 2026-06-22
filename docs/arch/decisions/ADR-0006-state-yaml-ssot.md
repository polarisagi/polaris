# ADR-0006: state.yaml 作为状态机 + 全模块阈值的单一权威源(SSoT)

- **状态**: Accepted（回填）
- **日期**: 2026-05-16
- **决策者**: 架构组
- **相关模块**: M4 / 全系统级(阈值定义)
- **实现详情**: [spec/state.yaml](../spec/state.yaml) 本身即 SSoT(§跳读 在文件头 14 行注释块)

## 上下文

polaris 多状态机(M4 FSM / M8 Task / M11 KillSwitch / M13 Lifecycle)+ 大量数值阈值。若散布在 Go const / Rust const / Markdown 表 / 监控配置,改一处忘改另一处即产生隐式 bug。

## 决策

**`docs/arch/spec/state.yaml` 是状态机 + 全模块阈值的单一权威源。**

- 跨模块共享的枚举、转移表、数值阈值集中定义
- Go 代码读取/复制时必须 cite `state.yaml §N`
- `spec_consistency_test`(执行带 2 golden 回归,ADR-0012 实施)强制 Go 侧与 state.yaml 同步

## 被驳与反例守护

| 方案 | 驳回理由 |
|------|---------|
| Go const 集中(`internal/protocol/constants.go`) | Rust FFI 侧不易读取;架构文档需手动对齐 |
| 多文件分模块定义 | 漂移风险高;跨模块关联难追 |
| Protobuf 定义 | 不易人工编辑;不便注释/文档;版本演进笨重 |
| JSON 定义 | 不支持注释;可读性差于 YAML |

**反例守护**:未来如有人在 Go 中硬编码新跨模块阈值—本 ADR 拒绝。必须先改 `state.yaml`,Go 代码引用。
