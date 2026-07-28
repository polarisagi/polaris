# ADR-0031: TTS 三路 Provider 架构（Edge / HTTP / Sherpa）

- **状态**: Accepted | **日期**: 2026-06-27 | **模块**: M13 Gateway / `internal/llm/tts/`

## 决策

`internal/llm/tts/` 引入 `Provider` 接口，三种实现按配置热切换，默认激活 `edge`：

| 实现 | 适用层 | 依赖 |
|------|--------|------|
| `*Engine`（Sherpa-ONNX Kokoro） | Tier-0 离线 | dylib + 82MB 模型 |
| `*EdgeProvider`（Edge TTS WS，默认） | Tier-0 起（需网络） | gorilla/websocket |
| `*HTTPProvider`（Sidecar） | Tier-1+（需 GPU） | 用户自维护 Python sidecar |

配置：`configs/defaults.toml [inference.tts] provider = "edge"`。

## 反例守护

拒绝把 CosyVoice 2 等 GPU 推理打包进主进程——必须以 HTTPProvider sidecar 形式独立运行。拒绝把 TTS 实现为 MCP 工具插件——TTS 是系统级自动播报行为，非用户显式触发的工具调用。

## 引用代码

`internal/llm/tts/{provider,edge,http,sherpa,wav}.go`
