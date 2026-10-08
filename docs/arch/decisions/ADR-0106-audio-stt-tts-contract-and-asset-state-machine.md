# ADR-0106: 语音 STT/TTS 修复：外部资产坐标契约化、ABI 钉死、Provider MIME 化、资产状态机

- **状态**: Accepted（Edge TTS、`model_precision`、STT 启动期下载与退避重试部分被 ADR-0107 取代；ABI 钉死/资产坐标契约/消除静默兜底仍有效）
- **日期**: 2026-10-01
- **决策者**: Opus（实测取证）/ Sonnet（实现）
- **相关模块**: M13 Gateway / `internal/llm/stt/` / `internal/llm/tts/` / `cmd/polaris/server_stt_tts.go`

## 上下文

macOS 15.8.1 Intel 实测：STT 引擎从未激活，Edge TTS 朗读 100% 失败。根因是一组互相掩盖的缺陷，且全部发生在"硬编码的外部假设"上，单测全绿。

STT：S1 sherpa 库资产名 6 平台中 5 个 404（`osx-x86_64`→`osx-x64`、`linux-x86_64`→`linux-x64`、`linux-aarch64`→`linux-aarch64-shared-cpu`、`win-x64-shared` 不存在）；S2 int8 模型 URL 404（实名 int8 在日期之前）；S3 int8 归档内是 `model.int8.onnx`，旧 mapper 只认 `model.onnx`，extractTar 因写出了 tokens.txt 判成功，`modelFilesPresent` 恒 false → 每次启动重下 166MB；S4 "HQ" fp32 归档实为 886MB（标点 fp32 279MB），内存充足即自动选 fp32 → 首次 1.16GB；S5 `use_itn=1` 丢首字错词（官方二进制同参数输出逐字节一致，是模型 ITN 路径缺陷）；S6 结果结构偏移错（clang 实测 v1.13.2：json=40, lang=48, emotion=56, event=64）；S7 `NewEngine` 库未加载返回空壳引擎、`Transcribe` 回出假 Mock 文本；S8 启动期一次性 fire-and-forget 下载，失败不可见不可恢复。

TTS（Edge）：T1 路径 `readspeaker` 应为 `readaloud`（400）；T2 缺 `Sec-MS-GEC`/`Sec-MS-GEC-Version`（403）；T3 `raw-24khz-16bit-mono-pcm` 不被支持（1007）；T4 `mstts:express-as` 不被支持（1007 "SSML is invalid"）。四层各自足以致命。另 `tts_edge` 内置工具失败时回出写死假 MP3 并报 success。

## 决策

1. **外部资产坐标必须有可执行校验**：`make audio-nettest`（`-tags nettest`）对全平台库 URL + defaults.toml 3 个 STT 模型 URL 做 HEAD 断言 200，并对 Edge 做真实合成（MIME=audio/mpeg 且 >1000 字节）。依赖外网，不进默认 CI；改动音频资产坐标或 Edge 协议必须手动跑。
2. **FFI 偏移钉死到编译期常量** `stt.SherpaABIVersion`（初稿 "1.13.2"，2026-10-08 起 "1.13.8"，见修订记录）：`sherpa_version` 非空且与之不同 → STT/TTS 资产初始化报错（不下载不加载）。升级版本必须重测全部偏移（识别器 608B、结果、标点 24B、Kokoro 448B）。
3. **库平台名表驱动，统一 `-lib` 变体**（darwin/arm64 `osx-arm64-shared-lib`、darwin/amd64 `osx-x64-shared-lib`、linux/amd64 `linux-x64-shared-lib`、linux/arm64 `linux-aarch64-shared-cpu-lib`、windows/amd64 `win-x64-shared-MT-Release-lib`）。
4. **默认 int8 全档**：`model_precision = "int8"|"fp32"`，fp32 仅显式 opt-in 且 FeatureHQSTT 开启；`use_itn` 默认 false。STT 与标点 mapper 分开，`model.int8.onnx`→`model.onnx`；EnsureAssets 解压后校验必需文件，缺失报带归档名的错误。
5. **消除静默兜底**：`NewEngine` 库未加载返回 `CodeUnimplemented` 错误；`Transcribe` 未初始化返回错误；删除 Mock 假文本；`tts_edge` 工具失败返回错误；日志不再出现 "mock engine"。
6. **TTS Provider 接口携带 MIME**：`Generate(ctx, text) (tts.Audio{Data, MIME}, error)`。Sherpa=`audio/wav`，HTTP=sidecar Content-Type（缺省 wav），Edge=`audio/mpeg`。`HandleAudioSpeech` 按 Provider 给出的 MIME 设 Content-Type。
7. **Edge 协议**：端点 `readaloud`；`Sec-MS-GEC` 按 edge-tts 7.2.8 算法（FILETIME 纪元、向下取整 300s、×1e7 tick、拼 TrustedClientToken 取 SHA256 大写），`Sec-MS-GEC-Version=1-{Chromium 完整版本}`（默认 `143.0.3650.75`，`inference.tts.edge_client_version` 可覆盖，微软升版本免发版）；403 时按响应 `Date` 头校正时钟偏差（包级 atomic）重试一次；输出格式 `audio-24khz-48kbitrate-mono-mp3` 直接作为 MP3 返回；SSML 恒为 `<voice><prosody>`；服务端 WS close 原因（如 1007）进错误信息。`edge_style` 配置废弃（保留字段兼容旧文件，非空非 default 时启动 Warn）。
8. **资产状态机**：`AudioAssetStatus{State disabled|pending|downloading|ready|failed, Detail, Error, UpdatedAt}`，STT/TTS 各一份（atomic）。STT 准备循环失败 → failed + 退避重试（1m/5m/15m/之后每 1h），failed 期间转写请求非阻塞唤醒一次立即重试。STT 未就绪时 `/v1/audio/transcriptions` 返回 503 JSON `{"error":"stt_not_ready","state","detail","message"}`；`GET /v1/system/capabilities` 增加 `stt_status`/`tts_status`（保留 `stt_available`）。状态切换打 slog；项目无低频状态 gauge 的既有惯例，不新造指标框架。
9. **旧配置迁移**：用户 `config.toml` 是旧模板副本，含错误默认值。`config.Load` 在叠加用户配置后，仅当 `sense_voice_model_url_std` / `punct_model_url` 与旧错误默认值精确相等时替换为新默认并 Warn，用户自定义值不动。
10. **前端**：开录前刷新 capabilities，`stt_status.state != ready` 则 toast 状态与 detail 且不开麦；分段上传失败同一次录音只 toast 一次；整段识别与朗读失败 toast 服务端 message。

## 后果

- **正向**: 外部坐标漂移由 audio-nettest 首红；偏移与库版本不再可被配置层解耦；失败对用户可见且可恢复；首次体验下载量从 1.16GB 降到约 230MB。
- **负向**: `audio-nettest` 不进 CI，靠"改动相关文件须手跑"的纪律；`-lib` 变体不含 sherpa 可执行文件与头文件（本项目只用 C API 动态库，不需要）；sherpa-onnx 1.13.2 的 libonnxruntime（arm64 与 x64）minos 为 15.5，macOS < 15.5 dlopen 必然失败（错误信息已附说明）。
- **反例守护**: 拒绝"为省事把 sherpa 版本做成可配置而不重测偏移"；拒绝"引擎未就绪时返回 Mock/假文本/假音频以保接口可用"（e81ff88d 的声明目标，本 ADR 补完残留）；拒绝"Provider 接口假设一律 WAV"；拒绝在 Edge 免费端点上重新引入 `mstts:express-as` 或 raw PCM 输出格式（服务端以 1007 拒绝，已实测）。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 默认 fp32（HQ 档） | 归档 886MB（+标点 fp32 279MB），无可测精度收益；int8 实测中文识别正确 |
| 保留 `use_itn=1` 并在后处理补首字 | 官方二进制同参数输出一致，属模型缺陷，在应用层补字是掩盖 |
| 前端轮询重试 STT 下载 | 重试属服务端资产生命周期，放前端会让失败状态与重试节奏分散到两处 |
| 把 Edge 输出 MP3 解码回 PCM/WAV 以保持"一律 WAV" | 引入 MP3 解码依赖；应让接口携带 MIME 而非迁就旧假设 |
| 新建 Prometheus gauge `polaris_audio_asset_state` | 指标集中注册并带基数守卫，无低频状态 gauge 的既有惯例；本次只落 slog |

## 引用代码

- `internal/llm/stt/downloader.go`（SherpaABIVersion、平台表、mapper、必需文件校验）
- `internal/llm/stt/sherpa.go`（偏移、错误化的 NewEngine/Transcribe、useITN）
- `internal/llm/tts/{provider,edge,http,sherpa}.go`
- `internal/gateway/server/chat/{audio_service,audio_status,provider}.go`
- `cmd/polaris/server_stt_tts.go`（准备循环与退避）
- `internal/config/config.go`（旧默认值迁移）
- `internal/llm/stt/assets_nettest_test.go`、`internal/llm/tts/edge_nettest_test.go`（契约）

## 重新评估触发条件

1. 升级 sherpa-onnx 版本：必须先用 clang offsetof 重测全部偏移并同步改 `SherpaABIVersion`，再更新本 ADR。
2. `make audio-nettest` 中 Edge 合成连续失败且 `edge_client_version` 覆盖无效：重议 Edge 免费端点作为默认 TTS（见 ADR-0031 追记）。
3. 出现可复现的 int8 模型识别精度退化且 fp32 修复：重议默认精度档位。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-10-01 | 初稿 |
| 2026-10-08 | 复核：sherpa-onnx 1.13.2→1.13.8。理由：1.13.2 捆绑的 ORT 1.24.4 在 macOS arm64 上比 1.13.8 的 ORT 1.28.2 慢约 3 倍（决定性实验：同一份 Go FFI 代码仅换动态库，SenseVoice RTF 1 线程 0.138→0.040、4 线程 0.040→0.0143；1.13.2 的 c-api 配新 ORT 即 0.040/0.022）。已按 1.13.8 c-api.h 对 STT/TTS/标点全部手写偏移与 sizeof 做 clang offsetof 复测（arm64 + x86_64 `_Static_assert`），与 1.13.2 逐项一致，偏移未改；五平台库清单（文件名/size/sha256）更新，新增 `sherpa-onnx.version` 标记使旧版本用户自动迁移。详见 ADR-0110 修订三 |
