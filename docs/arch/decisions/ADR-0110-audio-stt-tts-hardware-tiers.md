# ADR-0110: 语音 STT/TTS：资产契约、按需预置与硬件三档

> 状态：Accepted ｜ 首次：2026-06-27（原 ADR-0031）｜ 最近复核：2026-10-12 ｜ 含原 ADR-0031 / 0106 / 0107 / 0108 / 0110（v4）
> 模块：M13 Gateway；`internal/llm/{stt,tts,audioassets,audiorun}`。来源标注格式「原 ADR-NNNN 决策N」，供旧代码注释 grep。

## 决策一：模型与 Provider（原 ADR-0107 决策 1、原 ADR-0110 决策 1、原 ADR-0031）
- STT 仅 SenseVoice int8（标点模型 int8）；`use_itn=false`（实测 `use_itn=1` 丢首字，官方二进制同参数输出一致，属模型缺陷，不在应用层补字）。`model_precision`/fp32 档已删除。
- 服务端 TTS 仅 MeloTTS zh_en fp32（`MeloModel()`，单说话人 sid=0，归档内 `model.int8.onnx` 是 133 字节占位，不用、不列必需文件）；`inference.tts.model` 与 `kokoro_sid` 已删除，旧键仅 Warn（`warnLegacyAudioKeys`）。
- TTS `Provider` 接口保留两实现：`sherpa`（默认）、`http`（外部 sidecar，GPU 推理如 CosyVoice 2 / Qwen3-TTS，高级可选）。
- 拒绝把 GPU 推理打包进主进程（必须 HTTP sidecar）；拒绝把 TTS 做成 MCP 工具（TTS 是系统级自动播报，非用户显式调用）。
- 后果边界：低配机器失去服务端朗读，前端系统语音兜底（决策八）。

## 决策二：硬件三档（原 ADR-0110 修订二；取代原 ADR-0107 决策 2 与原 ADR-0110 初稿三档）
只看稳定画像（架构 + 逻辑核 + 总内存），不看空闲内存。阈值权威源 `internal/llm/audiorun/support.go`（`sttMinTotalMB=1800`、`sttMinCores=2`、`ttsMinTotalMB=3600`），与 CLAUDE.md `[Tier-0]` 一致。

| 档 | 条件 | STT | 服务端 TTS（Melo） | 后台预置 |
|---|---|---|---|---|
| A | 总内存 < 1800MB 或逻辑核 < 2 | 关 | 关 | 无 |
| B | 非 A 且总内存 < 3600MB | 开 | 关（系统语音） | 仅 STT |
| C | 总内存 ≥ 3600MB 且逻辑核 ≥ 2 | 开 | 开，RTF ≤ 0.8 基准门控 | STT → Melo |

- 原因码：A 档 `insufficient_ram`/`insufficient_cores`；B 档 TTS 专用 `tts_ram_tier`；基准定论过慢 `too_slow`；另有 `unsupported_platform`。
- 2GB 上 STT（空闲门槛 600MB）与 Melo（800MB）无法共存，共存只会让后加载者随机 503；朗读有零成本替代（前端系统语音在用户本地合成），语音输入无离线替代，故 2GB 保 STT 舍 TTS。
- 核数不预判 TTS（Melo 2 线程 RTF 0.42，M1），弱核交基准兜底。线程 `ttsThreads = min(4, 逻辑核)`。
- 加载时空闲内存门槛 `sttMinFreeMB=600`、`ttsMinFreeMB=800`：只决定「此刻能否加载」，不参与「是否支持」；不足时按决策七返回 503。
- 用户文案：B 档「朗读使用系统语音（服务端朗读需 4GB 内存）；语音输入可用」；A 档「本机配置不足（需 2GB 内存、2 核）」；`too_slow`「本机 CPU 合成速度不足，朗读使用系统语音」。

## 决策三：TTS 合成参数（原 ADR-0110 决策 6，取代原 ADR-0107 决策 6）
- `max_num_sentences = 100`（`internal/llm/tts/sherpa.go` `maxNumSentences`）：sherpa 按标点（含逗号）切子句，`=1` 时短子句被吞字（「我说另外，请打开。」回读为「你拿开」）。
- 服务端 `SplitSentences` 按句末标点（`。！？!?；;` 与换行）切句、逐句合成、拼接 PCM，峰值内存只取决于最长一句（20s 长段一次合成实测 ≈810MB，单句 ≈530MB）；句间补 `interSentenceSilence=0.12`。
- 输出 44.1kHz WAV。词典外英文长词逐字母拼读（Kubernetes→K U B…）是已知局限。
- 反例：拒绝 `max_num_sentences=1`；拒绝整段长文本一次送入。

## 决策四：TTS 基准门控、指纹与抗争用（原 ADR-0107 决策 2/5、原 ADR-0108 D2）
- 阈值 `MaxTTSRTF=0.8`：前端按句预取、边播边合成，RTF<1 不断流，0.8 留 20% 余量（旧 0.7 在空闲 i9-9880H 实测 0.65，贴线过严）。
- 基准句固定；1 次预热（丢弃）+ 2 次计时取最小 RTF。结果连同硬件指纹（`Fingerprint`：arch + 逻辑核 + 总内存 GiB + 模型名 `melo`）持久化到 `preferences` 键 `audio.tts_bench`（含模型名，防 Kokoro 时代结论被复用）。同指纹 supported 结论始终复用。
- 抗争用：基准前采样 CPU，连续 3 次 < 50% 视为空闲（预置路径最多等 10 分钟，用户触发立即测）；基准前后任一 ≥ 50% 记 `Contended=true`（`benchIdleThreshold=50`）。

| 结果 | 处置 |
|---|---|
| RTF ≤ 0.8 | `Supported=true`，持久化复用 |
| RTF > 0.8 且 Contended | 不落定论，`RetryOnStart=true`，状态 `ready`，退系统语音，等下个空闲窗口重测 |
| RTF > 0.8 且无争用 | 首次给一次重测；重测仍超标才定论 `Supported=false, RetryOnStart=false` |

- 实测依据：空闲 RTF 0.65 vs 嵌入 runner 占满 CPU 时 1.07；Kokoro 空闲 0.42 vs 安装后争用 1.10。
- 拒绝单次 RTF 超标即定论；拒绝用空闲内存判定是否支持；拒绝复用不含模型名的指纹。

## 决策五：下载前代理测速与磁盘回收（原 ADR-0110 修订三）
- Melo 资产约 167MB，下载前用已装 SenseVoice 对固定噪声（10s、16kHz、幅度 0.01、固定种子）测速：1 预热 + 3 计时取最小，线程 = Melo 线程数，临时 STT 引擎测完释放。`predicted = sttRTF × proxyRatio`，`proxyRatio=11.2`，`predicted > 1.0`（sttRTF > 0.0893）判过慢（`internal/llm/audiorun/proxy.go`）。1.0 比真实基准 0.8 宽松，边缘机型交真实基准。
- 系数标定（sherpa 1.13.8 + ORT 1.28.2，M1，Go 管线 t=1/2/4 各 3 轮）：meloRTF/sttRTF 最小 12.45，×0.9 ≈ 11.2。1.13.2 时代系数 4.35、Python 管线系数 13 均已作废（管线不可比）。升级 sherpa/ORT 后必须重标。
- 过慢：不下载，TTS 置 `unsupported/too_slow`，落库 `audio.tts_bench.melo`（`method:"proxy"`），预置器尊重，同指纹重启直接生效。争用（测前后 CPU ≥ 50%）、STT 资产缺失/内存不足/加载失败：无结论，走原路径（下载 + 真实基准），不落库。
- 逃生口：用户主动 `POST /v1/audio/tts/install` 无视代理结论，真实结果覆盖代理记录（代理记录不算已重测）。
- 回收：真实基准定论过慢（非争用、已重测、`RetryOnStart=false`）后删除 `models/tts/melo/`，状态保持 `unsupported/too_slow`；启动恢复读到同指纹真实定论 too_slow 而目录仍在时补回收；争用造成的暂时不可用绝不删。回收只删目录名恰为 `melo` 的确切路径，发生在 Slot 加载回调内、引擎 `Close` 之后；`slow` 先于删除置位，`gate()`/`loadEngine` 据此拒绝后续加载；`assetMu` 使下载/解压与回收互斥；`installing` CAS 使预置器与手动安装互斥。
- 可观测（HE-1）：slog 记录 stt_rtf/predicted/contended/回收；计数器 `polaris.audio.tts_proxy_bench_total`、`tts_proxy_too_slow_total`、`tts_proxy_skipped_total`、`tts_reclaim_total`。

## 决策六：后台串行预置、懒加载与空闲卸载（原 ADR-0108 D1/D5，取代原 ADR-0107 决策 3；原 ADR-0107 决策 4 保留）
- `inference.audio.auto_install = true`（默认）：守护进程启动后延迟 45s（避开向量回填与 runner 预热），由单一 Provisioner **串行**执行 STT → TTS，仅预置本机档位支持的能力。A 档不预置。
- 预置器只下载校验，不常驻引擎；TTS 首次基准后立即卸载。失败退避 5m → 30m → 2h，进程内最多自动重试 3 次，耗尽保持 `failed` 并提供手动重试；手动点击与预置共享原子锁。
- 引擎懒加载（并发共享一次加载，30s 等待超时但后台继续），`inference.audio.idle_unload_minutes`（默认 10，0 不卸载）后卸载，引用计数保证使用中不被关闭。
- `auto_install=false` 时首次使用经 `POST /v1/audio/{stt|tts}/install` 异步安装（202/200/422/503），前端用页内 modal 确认，禁 `window.confirm`。
- 拒绝并行安装 STT 与 TTS（带宽与解压互抢，污染基准）；拒绝启动同步下载。

## 决策七：资产坐标契约、sherpa ABI 钉死与消除静默兜底（原 ADR-0106 决策 1~5/8/9）
- 外部资产坐标必须可执行校验：`make audio-nettest`（`-tags nettest`，`internal/llm/audioassets/nettest_test.go`）对全部资产 URL 做 HEAD 断言 200 并核对 Content-Length。依赖外网，不进 CI；改 `internal/llm/stt`/`internal/llm/tts` 下载 URL/资产名、`configs/defaults.toml [inference.stt|tts]` 模型 URL、`audioassets` 清单后必须手跑（实例：硬编码外部假设曾让 6 平台 5 个库 URL 404、int8 模型 URL 404、mapper 只认 `model.onnx` 致每启动重下 166MB，单测全绿）。
- 资产清单 `audioassets/manifest.go` 带 sha256 + size，下载后强校验，缓存坏归档自动丢弃重下一次；解压后校验必需文件，缺失报带归档名的错误。库平台名表驱动，统一 `-lib` 变体：darwin/arm64 `osx-arm64-shared-lib`、darwin/amd64 `osx-x64-shared-lib`、linux/amd64 `linux-x64-shared-lib`、linux/arm64 `linux-aarch64-shared-cpu-lib`、windows/amd64 `win-x64-shared-MT-Release-lib`。
- FFI 偏移钉死到编译期常量 `stt.SherpaABIVersion`（= `audioassets.SherpaABIVersion` = "1.13.8"）：`sherpa_version` 非空且不同 → STT/TTS 资产初始化报错（不下载不加载）。升级必须先用 clang `offsetof`/`_Static_assert`（arm64 + x86_64）重测全部手写偏移与 sizeof（识别器配置 608B、识别结果 json=40/lang=48/emotion=56/event=64、标点配置 24B、TTS 配置 448B、模型配置 416B），再改常量与 `defaults.toml sherpa_version`、清单五平台 size/sha256，并重标决策五系数。
- 库版本标记 `sherpa-onnx.version`（`libMarkerName`）：库存在且标记等于 `SherpaABIVersion` 才算已安装，旧版用户自动重下覆盖（并清理遗留 `libonnxruntime.1.24.4.dylib`）。
- 消除静默兜底：`NewEngine` 库未加载/必需文件缺失返回错误（`CodeUnimplemented`）；`Transcribe` 未初始化返回错误；无 Mock 假文本/假音频。
- 双 ORT 隔离（向量化引擎 ADR-0109 与 sherpa 各带不同版本 ORT）：`stt.Dlopen` 与 `embedonnx.dlopen` 用 `RTLD_LOCAL`；Windows 向量化 ORT 落地名为 `onnxruntime_embed.dll`。Linux/Windows 未实跑。
- 旧配置迁移：`config.Load` 在叠加用户配置后，仅当 `sense_voice_model_url_std`/`punct_model_url` 与旧错误默认值精确相等时替换为新默认并 Warn；旧键（`model_precision`、`edge_*`、`model_url`、`tokens_url`、`kokoro_sid`、`model`）仅 Warn 忽略；`provider="edge"` 迁移为 `sherpa`。
- 拒绝把 sherpa 版本做成可配置而不重测偏移；拒绝引擎未就绪时返回 Mock/假文本/假音频。

## 决策八：资产状态机、错误契约、Provider MIME 与前端（原 ADR-0106 决策 6/8/10、原 ADR-0107 决策 7/8/10、原 ADR-0108 D3/D4）
- 状态机 `AudioAssetStatus{State, Detail, Error, UpdatedAt}`（STT/TTS 各一份，atomic），State 含 disabled/not_installed/pending/downloading/ready/failed/unsupported 等，含 `progress` 与 TTS `model`（`melo`/`none`）。状态切换打 slog；不新造低频状态 gauge。
- 未就绪时 `/v1/audio/transcriptions` 与 `/v1/audio/speech` 返回 503 JSON `{error: "stt_not_ready"|…, state, detail, message}`（`internal/gateway/server/chat/audio_service.go`）。`GET /v1/system/capabilities` 带 `stt_status`/`tts_status`（保留 `stt_available`）；`GET /v1/audio/status` 供前端轮询。
- `Provider.Generate(ctx, text) (tts.Audio{Data, MIME}, error)`：Sherpa=`audio/wav`，HTTP=sidecar Content-Type（缺省 wav）；`HandleAudioSpeech` 按 MIME 设 Content-Type。拒绝 Provider 假设一律 WAV。
- 前端系统语音兜底：服务端 TTS 不支持/失败/下载中/基准未过，或 `inference.tts.engine=system`（`auto|server|system`）时用 `speechSynthesis`，仅限 `voice.localService && lang 以 zh 开头` 的本地语音，无则明确提示不可用；严禁云端外送语音。服务端 `unsupported` 且系统语音可用时不弹 toast，原因文案只在状态 chip 下拉（`info` 级，i18n）。
- 前端 `audio` store 统管状态，进度集中于底部状态栏单一 chip，toast 支持 `key` 去重；开录前刷新 capabilities，`stt_status.state != ready` 则提示且不开麦；分段上传失败同一次录音只 toast 一次。
- 麦克风安全上下文：`getUserMedia` 需 `127.0.0.1`/`localhost` 或 HTTPS（远程 VPS 必须 HTTPS）；Tauri macOS 需 `NSMicrophoneUsageDescription`（`desktop/src-tauri/Info.plist`）；Windows WebView2 首次弹系统权限；Linux WebKitGTK 支持有限，缺失走「不可用」提示。

## 已驳方案 / 沿革（取代与推翻保留一行）
| 日期 | 方案/变更 | 驳回或取代理由 | 原编号 |
|---|---|---|---|
| 2026-06-27 | Edge TTS WS（曾为默认） | 2026-10-01 实测免费端点：需 `Sec-MS-GEC`、路径 `readaloud`、raw PCM 与 `mstts:express-as` 均 1007 拒绝；2026-10-02 整条删除（云端外送违背离线原则） | 0031、0106、0107 |
| 2026-10-02 | Kokoro v1.1 fp32 服务端 TTS（int8 无 VNNI 机 RTF 1.44–1.76 慢于实时） | 2026-10-04 盲听中文音色最差，改 Melo；旧 `models/kokoro/` 在 Melo 校验后清理 | 0107、0110 |
| 2026-10-04 | Matcha zh-en + vocos-16k（RTF 0.063，听感第二；曾作 B 档/降级链） | 2026-10-08 删除：训练数据源自标贝 CSMSC（非商业条款），与 Apache-2.0 声明冲突，链路不可核验；归档附带 GPL-3.0 `espeak-ng-data`；开源自托管不得默认下发。旧 Matcha（vocos-22k）另因英文含糊淘汰。`models/tts/matcha/` 同样清理 | 0107、0110 |
| 2026-10-02 | Piper、Supertonic 3、ZipVoice-distill int8 | 英文词丢失 / 不支持中文 / RTF 0.34 且需参考音频 | 0107、0110 |
| 2026-10-01 | STT 启动期 fire-and-forget 下载 + 退避循环；默认 fp32 HQ 档（886MB + 标点 279MB） | 失败不可见；无可测精度收益。先改按需下载（0107），再改后台串行预置（决策六） | 0106、0107、0108 |
| 2026-10-02 | 旧门槛：服务端 TTS 4GB/4 核；RTF 阈值 0.7 | 改三档（决策二）与 0.8（决策四） | 0107 |
| 2026-10-08 | 初稿两档/三档（A/B Matcha/C Melo，B 档需 4 核） | 被 Matcha 删除与修订二取代 | 0110 |
| 2026-10-08 | sherpa-onnx 1.13.2 → 1.13.8 | 1.13.2 捆绑 ORT 1.24.4 在 macOS arm64 慢约 3 倍（同一 Go FFI 换库 RTF 0.138→0.040）；偏移复测一致；macOS 最低系统要求 15.5 随之消失 | 0106、0110 |
| 2026-10-01 | 前端轮询重试 STT 下载；Edge MP3 解码回 WAV；新建 `polaris_audio_asset_state` gauge；应用层补 ITN 丢字；FFmpeg 后处理；macOS 系统语音作默认 | 重试属服务端生命周期；引入解码依赖；无低频 gauge 惯例；掩盖模型缺陷；只改音色不改韵律且带许可负担；精简版音质差仅 Mac | 0106、0110 |
| 2026-10-02 | 并行安装 STT/TTS；toast 轮询进度；`window.confirm`；常驻引擎 | 互抢污染基准；刷屏；Tauri WebView 表现不一；空闲占 400–600MB | 0107、0108 |

## 重新评估触发条件
1. 升级 sherpa-onnx：先重测 ABI 偏移并重标代理系数，再更新本文。
2. 代理 predicted 与真实 Melo RTF 在空闲机器上持续偏差 > 2 倍：重标系数 11.2。
3. sherpa 上游修复子句拆批吞字，或 Melo 提供英文 G2P 回退。
4. 出现 RTF 与音质均优于 Melo、单句 RSS ≤ 600MB 且许可证链路可核验的中文模型。
5. int8 SenseVoice 出现可复现精度退化且 fp32 修复：重议默认精度。
6. 仓库引入跨平台磁盘空间探针，或 <2GB 硬件需默认关闭 `auto_install`：重议决策六。

## 引用代码
- `internal/llm/audiorun/{support,bench,proxy,provisioner,slot,status,tts_service,stt_service}.go`
- `internal/llm/audioassets/{manifest.go,nettest_test.go}`
- `internal/llm/stt/{downloader,sherpa,dlopen_unix,dlopen_windows}.go`
- `internal/llm/tts/{provider,sherpa,models,http,downloader}.go`
- `internal/gateway/server/chat/{audio_service,audio_status,provider}.go`、`internal/gateway/server/server_routes.go`
- `cmd/polaris/server_stt_tts.go`、`internal/config/config.go`、`configs/defaults.toml`
- `web/src/js/store/{audio,chat}.js`、`desktop/src-tauri/Info.plist`
