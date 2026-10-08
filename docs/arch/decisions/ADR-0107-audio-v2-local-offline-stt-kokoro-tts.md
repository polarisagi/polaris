# ADR-0107: 语音 v2：本地离线 STT + Kokoro TTS、按需下载、最低配置门控、系统语音兜底

- **状态**: Accepted（决策 3 与反例守护第一条被 ADR-0108 取代；决策 1 的 TTS 部分、决策 2 的服务端 TTS 门槛、决策 6 与"拒绝重新引入 Matcha"反例守护被 ADR-0110 取代）
- **日期**: 2026-10-02
- **决策者**: 用户（定位与模型裁决）/ Opus（实测）/ Sonnet（实现）
- **相关模块**: M13 Gateway / `internal/llm/audioassets/` / `internal/llm/audiorun/` / `internal/llm/stt/` / `internal/llm/tts/` / `web/src/js/store/chat.js`
- **取代**: ADR-0106 中 Edge TTS（决策 6/7 的 Edge 部分）、`model_precision`、STT 启动期自动下载与退避循环；ADR-0031 多 Provider 中的 Edge/云端 Provider。ADR-0106 的 ABI 钉死、资产坐标契约、消除静默兜底仍有效。

## 上下文

定位变更：产品主形态是桌面端 + 浏览器；同一守护进程也可部署在 VPS，核心路径最低 2GB。语音必须离线、可降级，且不能在启动期给低配机器塞下载与常驻内存。

实测（i9-9880H 8C/16GB，无 VNNI，sherpa-onnx 1.13.2，2 线程除注明）：

| 模型 | 体积 | RTF | RSS | 中英混读（ASR 回读） | 裁决 |
|---|---|---|---|---|---|
| Piper zh xiao_ya int8 | 14MB | 0.29 | 153MB | 英文词整个丢失 | 淘汰 |
| Matcha zh-en + vocos-22k | 79+18MB | 0.044 | 186MB | 英文词含糊（GitHub→"g hop"） | 淘汰（听感） |
| Kokoro v1.1 int8 | 147MB | 1.44–1.76 | 390MB | 好 | 淘汰（慢于实时） |
| Kokoro v1.1 fp32 | 365MB | 0.59（4 线程 0.42） | 600MB | 好 | 采用 |
| Supertonic 3 | 129MB | - | - | 不支持中文 | 淘汰 |
| SenseVoice int8（ASR） | 166MB | 0.053 | 420MB | 基线 | 采用 |
| FunASR-nano int8（ASR） | ~190MB | 0.044 | 460MB | 英文/方言略好 | 暂不采用（同配置可零代码替换） |

## 决策

1. **模型**：STT 仅 SenseVoice int8（标点 int8）；服务端 TTS 仅 Kokoro v1.1 fp32，默认 `kokoro_sid=3`。Edge、Matcha、Piper 等全部移除。HTTP sidecar Provider 保留为可选高级项。
2. **最低配置**：STT 2GB 内存 + 2 逻辑核心；服务端 TTS 4GB + 4 核；首次加载基准 RTF > 0.8 判不支持（阈值理由：前端按句预取、边播边合成，RTF<1 即不断流，0.8 留 20% 余量；原定 0.7 在空闲 i9-9880H 上实测 0.65，几乎贴线，过严）。"是否支持"只看稳定的硬件画像（架构 + 核数 + 总内存，同时作为基准指纹）；空闲内存只决定"现在能否加载"（STT ≥ 600MB，TTS ≥ 900MB）。
3. **按需下载**：启动不下载任何语音资产；状态 `not_installed` 起步，已存在的资产识别为已安装但未加载。用户确认后经 `POST /v1/audio/{stt|tts}/install` 异步安装（202/200/422/503），进度由 `*_status.progress` 轮询。所有资产在 `audioassets` 清单中带 sha256 + size，下载后强校验，归档缓存的坏文件自动丢弃重下一次。
4. **懒加载 + 空闲卸载**：引擎首次使用时加载（并发共享一次加载，30s 等待超时但后台继续），`inference.audio.idle_unload_minutes`（默认 10，0 为不卸载）后卸载，使用中引用计数保证不被关闭。
5. **TTS 首次基准**：加载后跑固定句"你好，这是一次语音合成速度测试，今天是二零二六年十月二日。"：1 次预热（丢弃）+ 2 次计时，取**最小 RTF**（滤掉偶发 CPU 抢占）。结果与硬件指纹持久化到 `preferences` 键 `audio.tts_bench`；supported 结论同指纹始终复用。unsupported 结论带 `retry_on_start:true`：下次守护进程启动后不直接采信，等用户再次触发朗读/安装时重测一次；重测仍过慢则 `retry_on_start` 置 false 定论（同指纹不再重测）。实测依据：空闲机器 RTF 0.65；Polaris 嵌入 runner（llama-server）占满 CPU 时 RTF 1.07——单次结论可能是瞬时负载所致。
6. **Kokoro 文本规整**：`rule_fsts=phone-zh.fst,date-zh.fst,number-zh.fst`，`max_num_sentences=1`，`silence_scale=0.2`，双词典（英 + 中）。结构偏移见 `internal/llm/tts/sherpa.go`（ConfigSize 448）。
7. **系统语音兜底（前端）**：服务端 TTS 不支持/失败/用户拒绝下载，或 `inference.tts.engine=system` 时用 `speechSynthesis`，仅限 `voice.localService && voice.lang.startsWith('zh')` 的本地语音，没有则明确提示不可用，不使用云端语音。`engine`：`auto|server|system`。
8. **错误契约**：未就绪时 `/v1/audio/speech` 与 `/v1/audio/transcriptions` 返回 503 JSON `{error,state,detail,message}`，不返回假音频或假文本。
9. **配置迁移**：旧键（`model_precision`、Edge 相关等）仅 Warn 不报错；`provider="edge"` 迁移为 `sherpa`。
10. **桌面/浏览器麦克风**：`getUserMedia` 需安全上下文（`127.0.0.1`/`localhost` 可用，远程 VPS 必须 HTTPS）。Tauri macOS 需 `NSMicrophoneUsageDescription`（`desktop/src-tauri/Info.plist`）；Windows WebView2 首次会弹系统权限；Linux WebKitGTK 的 `getUserMedia`/`speechSynthesis` 支持有限，缺失时走"不可用"提示。

## 后果

- **正向**: 启动零下载零常驻；低配机器不被拖垮且有明确降级；首次基准用实测而非猜测判定 TTS 可用性；资产完整性可验证。
- **负向**: 服务端 TTS 要求 4GB/4 核，2GB VPS 只剩 STT + 浏览器系统语音；首次使用有一次下载等待；基准会占用数秒 CPU。
- **反例守护**: 拒绝启动期自动下载语音资产；拒绝用空闲内存判定"是否支持"（会随负载抖动）；拒绝在服务端不可用时回退云端语音或返回占位音频；拒绝重新引入 Edge/Matcha/Piper/int8 Kokoro（实测见上表）。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 阈值 0.7 | 空闲 i9 实测 0.65 贴线；预取播放只需 RTF<1，改 0.8 |
| Matcha 作默认（RTF 0.044） | 速度最佳但英文词含糊，技术词汇场景不可接受 |
| Kokoro int8 | 无 VNNI 机器 RTF 1.44–1.76，慢于实时 |
| 启动期预下载 | 违反 2GB 核心路径与离线按需原则 |
| 常驻引擎 | 空闲时占用 400–600MB，改为懒加载 + 空闲卸载 |

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-10-02 | 初稿 |
| 2026-10-02 | 决策 3（按需下载）与反例守护第一条（拒绝启动期自动下载语音资产）被 ADR-0108 取代为后台串行预置 |
| 2026-10-08 | 决策 1(TTS)/2(TTS 门槛)/6 与 Matcha 反例守护被 ADR-0110 取代：Kokoro 改为 MeloTTS，服务端 TTS 门槛改为硬件三档（A 全关 / B 只开 STT / C STT+Melo，Matcha 同日修订删除） |
