# ADR-0031: TTS 三路 Provider 架构（Edge / HTTP / Sherpa）

- **状态**: Accepted
- **日期**: 2026-06-27
- **决策者**: MrLaoLiAI
- **相关模块**: M13 Gateway / `internal/llm/tts/`

## 上下文

原 TTS 实现（`internal/llm/tts/sherpa.go`）固化为单一后端：Sherpa-ONNX + Kokoro multi-lang v1.1。Kokoro 是英文优先模型，中文声线（zf_001）音质明显偏差，中文用户体验差。引入更好中文 TTS 有三条可行路径：

1. **本地换模型**（Sherpa-ONNX + MeloTTS/VITS）：无外部依赖，但需改 C struct 布局代码，且 Kokoro 音质天花板已触及。
2. **云端免费 API**（Microsoft Edge TTS WebSocket）：免费、无密钥、中国大陆可访问，中文质量优秀（晓晓 Neural），零 Python 依赖。
3. **GPU 推理 sidecar**（CosyVoice 2 / Qwen3-TTS）：中文顶级质量，但需要 Python 环境 + GPU，Tier-0 VPS 不可用。

核心矛盾：三条路针对不同硬件层的用户，单一实现无法覆盖全部场景。需要 Provider 接口抽象使三者可配置并存，运行时由 `configs/defaults.toml` 决定激活哪条路。

## 决策

在 `internal/llm/tts/` 引入 `Provider` 接口，使三种后端实现可通过配置热切换；默认激活 `edge`（Edge TTS），无需用户安装任何模型。

**接口定义**（`provider.go`）：
```go
type Provider interface {
    Generate(ctx context.Context, text string) ([]byte, error) // 返回 WAV
    Close() error
}
type ProviderBox struct{ P Provider } // 用于 atomic.Pointer[ProviderBox]
```

**三种实现**：

| 实现 | 文件 | 适用层 | 依赖 |
|------|------|--------|------|
| `*Engine`（Sherpa-ONNX Kokoro）| `sherpa.go` | Tier-0（离线） | sherpa-onnx dylib + 82MB 模型 |
| `*EdgeProvider`（Edge TTS WS）| `edge.go` | Tier-0 起（需网络） | `gorilla/websocket`（已在 go.mod） |
| `*HTTPProvider`（HTTP sidecar）| `http.go` | Tier-1+（需 GPU） | 用户自行启动 Python sidecar |

**配置切换**（`configs/defaults.toml`）：
```toml
[inference.tts]
provider = "edge"                          # ""/"sherpa" | "edge" | "http"
edge_voice = "zh-CN-XiaoxiaoNeural"       # Edge provider 声线
http_endpoint = ""                         # HTTP provider sidecar 地址
```

**原子持有**：`ChatHandler.TTSEngine` 从 `*atomic.Pointer[tts.Engine]` 改为 `*atomic.Pointer[tts.ProviderBox]`，规避 `atomic.Value` 要求同一具体类型的限制。

**WAV 编码拆分**：`encodeWAV`（float32→WAV，Sherpa 用）和 `encodeWAVFromPCM16`（PCM16→WAV，Edge 用）统一移至 `wav.go`，消除重复。

**Server 路由**（`InitTTSEngine`）：
- `edge`/`http`：同步注册，无 FeatureGate 门控（无内存开销），立即可用
- `sherpa`：保持原有异步下载 + FeatureLocalTTS 门控

## 后果

- **正向**：
  - Tier-0 中文用户开箱即得高质量语音（Edge TTS，晓晓 Neural），零配置
  - GPU 用户可接入 CosyVoice 2 / Qwen3-TTS 等顶级中文模型，仅改一行配置
  - 主二进制文件零新增 Python 依赖，编译输出体积不变
  - Provider 接口为未来 Azure / 阿里云 / 火山引擎 TTS 留扩展点

- **负向**：
  - Edge TTS 依赖微软服务可用性（`speech.platform.bing.com`），断网环境降级为 Sherpa
  - Edge TTS 协议为非官方 WebSocket（Edge 浏览器内置 TrustedClientToken）；API 变更时需更新
  - HTTP sidecar 需用户自行维护 Python 环境，不在 Polaris 进程管控范围内

- **反例守护**：
  - 未来若有人提议"把 CosyVoice 2 打包进 Polaris 主进程"——本 ADR 明确拒绝：GPU 推理服务必须以 sidecar 形式独立运行，通过 HTTPProvider 对接，保持主进程纯净。
  - 未来若有人提议"把 TTS 实现为 MCP 工具插件"——本 ADR 明确拒绝：TTS 是系统级生命周期行为（自动播报 AI 回复），不是用户显式触发的工具调用，不属于插件系统管辖。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 单纯换 Sherpa-ONNX 模型（MeloTTS/VITS）| 需额外支持 VITS C struct 布局（Sherpa 中 Kokoro/VITS 两套接口），代码改动量与质量收益不成比例；仍受本地模型音质上限约束 |
| CosyVoice 2 直接嵌入主进程 | 需要 PyTorch/Python 环境，违反"不引第三种语言"硬约束；主进程引入 C-ext Python 解释器破坏 Tier-0 内存上限 |
| OpenAI TTS API（tts-1-hd）| 中国大陆无法直连；需要付费 API Key，不适合默认路径 |
| `tts_edge` MCP 工具（已有实现）| 该实现走 shell exec 调用 Python edge-tts CLI，是 AI 主动调用工具；系统自动朗读回复需要内部 Provider 机制，两者用途不同，不可复用 |

## 引用代码

- `internal/llm/tts/provider.go` — Provider 接口 + ProviderBox
- `internal/llm/tts/edge.go` — EdgeProvider（gorilla/websocket，WebSocket 协议实现）
- `internal/llm/tts/http.go` — HTTPProvider（通用 HTTP sidecar 适配器）
- `internal/llm/tts/sherpa.go` — 原 Engine 现实现 Provider 接口
- `internal/llm/tts/wav.go` — encodeWAV / encodeWAVFromPCM16 共享编码
- `internal/gateway/server/chat/audio.go` — SetTTSEngine(tts.Provider) 原子注入
- `internal/gateway/server/server.go` — InitTTSEngine 三路路由
- `internal/config/config_types.go` — TTSConfig（Provider/EdgeVoice/HTTPEndpoint 字段；2026-07-08 随 R7 拆分从 `config.go` 移出）
- `docs/arch/M13-Interface-Scheduler.md §8.4` — 语音输入/输出组件表

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-06-27 | 初稿 |
