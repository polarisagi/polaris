# ADR-0110: 语音 v4：MeloTTS 取代 Kokoro，按硬件三档开放语音能力

- **状态**: Accepted
- **日期**: 2026-10-08
- **决策者**: 用户（试听裁决）/ Opus（实测与设计）/ Sonnet（实现）
- **相关模块**: M13 Gateway / `internal/llm/audioassets/` / `internal/llm/audiorun/` / `internal/llm/tts/` / `internal/config/` / `cmd/polaris/server_stt_tts.go` / `web/src/js/store/`
- **取代**: ADR-0107 决策 1 的 TTS 部分（Kokoro v1.1 fp32）、决策 2 的服务端 TTS 门槛、决策 6（Kokoro 文本规整）、反例守护中"拒绝重新引入 Matcha"（该条已被本 ADR 2026-10-08 修订再次生效：Matcha 因许可证链路不可核验被删除）；ADR-0108 D3 中"Kokoro 就绪"的表述。ADR-0107 的 STT 选型、懒加载/空闲卸载、错误契约，ADR-0108 的串行预置、抗争用基准、前端单一进度指示继续有效。

## 上下文

用户反馈 Kokoro 朗读中文音色差。2026-10-04 在 Mac mini M1（8 核/16GB，sherpa-onnx 1.13.8 Python 绑定，与 1.13.2 C API 同模型格式）上用同一段中英混排文本盲听 5 个方案：macOS 婷婷（精简版）、Kokoro v1.1 zf_001、ZipVoice-distill int8、MeloTTS zh_en、Matcha zh-en。**用户裁决：MeloTTS 真人感最强，Matcha 次之，二者音质最好**。

新事实（推翻 ADR-0107 "拒绝 Matcha" 的依据）：ADR-0107 实测的是旧版 Matcha + vocos-22k，英文词含糊（GitHub→"g hop"）。本次为 `matcha-icefall-zh-en`（上游 modelscope `dengcunqin/matcha_tts_zh_en_20251010`）+ `vocos-16khz-univ`，SenseVoice 回读：`DOCKER`、`MAKE LINT`、`REQUEST`、`THIS MODEL ... BOTH ENGLISH AND CHINESE` 均正确，长技术词（Kubernetes/GitHub）略含糊。

实测（M1，SenseVoice 回读，RSS 为进程峰值减去 Python 基线 18MB）：

| 模型 | 下载体积 | RTF 1/2/4 线程 | 单句 RSS | 20s 长段一次合成 RSS | 英文 | 用户听感 |
|---|---|---|---|---|---|---|
| MeloTTS zh_en fp32（44.1kHz） | 167MB | 0.79 / 0.42 / 0.23 | ≈530MB | ≈810MB | 短词好；词典外长词逐字母拼读（Kubernetes→K U B…） | 第 1 |
| Matcha zh-en + vocos-16k（16kHz；**2026-10-08 修订已删除：许可证链路不可核验**） | 79MB + 54MB | 0.063 / 0.035–0.040 / - | ≈280MB | ≈345MB | 可懂 | 第 2 |
| Kokoro v1.1 fp32（现状） | 330MB | - / - / - | ≈600MB（ADR-0107） | - | 最好 | 差 |

**MeloTTS 致命缺陷与规避**：sherpa-onnx 按标点（含逗号）切分子句，`max_num_sentences=1` 时每个子句单独合成，短子句会被吞字——"我说另外，请打开。"回读为"你拿开"，"另外，…"句首"另外"丢失。`max_num_sentences=100`（整句同批）后回读完全正确。代价是长文本一次合成峰值内存随长度增长（上表 810MB），故服务端须按句末标点自行分句、逐句合成后拼接。

## 2026-10-08 修订：删除 Matcha，服务端 TTS 只保留 MeloTTS

**裁决**：删除 Matcha（含 `vocos-16khz-univ` 声码器与降级链 Melo→Matcha），服务端 TTS 只保留 MeloTTS。下文「决策」正文保留作历史，被取代的条目以"（已被 2026-10-08 修订取代）"标出；以本节为准。

理由：

1. **许可证链路不可核验**：`matcha-icefall-zh-en` 来自 modelscope `dengcunqin/matcha_tts_zh_en_20251010`，其元数据声明 Apache-2.0，但 README 写明从 `dengcunqin/matcha_tts_zh_en` 微调，而后者 README 写"使用 icefall 例子下的 baker tts 模型训练而成"——即源自标贝 DataBaker 中文标准女声音库（CSMSC，非商业使用条款）训练的模型，训练数据未公开。Apache-2.0 声明与上游数据条款冲突，链路不可核验。另外 Matcha 归档附带 `espeak-ng-data`（GPL-3.0）。polaris 是开源自托管项目，默认下发来源存疑的权重不可接受。MeloTTS 为 MyShell MIT（"free for both commercial and non-commercial use"），且不依赖 `espeak-ng-data`。
2. **系统更简**：单模型 = 单资产、单 FFI 路径、单基准、无降级分支；音质为用户裁决第一。低配机器失去的只是"服务端朗读"，前端系统语音本就是兜底（VPS 场景语音在用户本地浏览器合成，不耗 VPS 资源）。

新的两档（取代决策 2 的三档表；**已被下方「修订二」的三档取代**）：

| 档 | 条件 | STT（SenseVoice） | 服务端 TTS | 后台预置（auto_install） |
|---|---|---|---|---|
| A 极低配 | 总内存 < 1800MB，或逻辑核 < 2 | 关 | 关，前端系统语音 | 不预置任何语音资产 |
| 支持档 | 其余 | 开 | MeloTTS（由抗争用基准 RTF ≤ 0.8 最终判定；定论 too_slow → 前端系统语音） | STT → MeloTTS 串行预置 |

- 加载时空闲内存门槛：STT 600MB、MeloTTS 800MB 不变。2GB 机器上两者不能同驻时，由门槛拒绝后加载者，返回现有 503 契约。线程：MeloTTS `min(4, 逻辑核)`。
- 配置 `inference.tts.model` 删除（只剩一个模型，保留是无意义的开关）；`kokoro_sid` 与 `model` 旧键出现时仅 Warn。
- 基准持久化键保留含模型名（`audio.tts_bench.melo`，指纹含 `model=melo`），防止 Kokoro 时代结论被复用。`/v1/audio/status` 的 `tts_status.model` 为 `melo`（可用/待安装）或 `none`（本机不支持/基准过慢）。
- 清理：新 Melo 资产校验成功后删除 `models/kokoro/`，同时删除修订前可能已下载的 `models/tts/matcha/`；只删这两个确切路径，失败 Warn。
- 被删除的代码：`MatchaModel()`、`MatchaVocoder()`、`KindTTSFile`、Matcha FFI 偏移与引擎构造、降级链与预置器第二轮。

## 2026-10-08 修订二：2GB 档关闭服务端 TTS，只开 STT

**裁决**：档位改为三档（只看稳定画像），取代上方修订的两档表：

| 档 | 条件 | STT | 服务端 TTS（Melo） | 后台预置 |
|---|---|---|---|---|
| A | 总内存 < 1800MB 或逻辑核 < 2 | 关 | 关 | 无 |
| B | 非 A 且总内存 < 3600MB | 开 | 关（前端系统语音） | 仅 STT |
| C | 总内存 ≥ 3600MB 且逻辑核 ≥ 2 | 开 | 开（ADR-0108 D2 基准 RTF ≤ 0.8 门控，定论 too_slow → 系统语音） | STT → Melo |

理由：

1. **2GB 上无法共存**：STT 空闲门槛 600MB + Melo 800MB 无法同驻，核心路径（最低 2GB）还要预算；共存只会让后加载者随机 503，行为不可预期。
2. **两者不对称**：朗读有零成本替代——前端系统语音在用户本地设备合成，不耗 VPS 资源；语音输入没有离线本地替代（浏览器语音识别多为云端，违反离线原则），因此 2GB 保 STT、舍服务端 TTS。STT 懒加载 + 空闲卸载 + 600MB 空闲门槛已有保护。
3. **TTS 核数门槛取 2 而非旧 Kokoro 的 4**：Melo 2 线程 RTF 0.42（M1），弱核机器由基准兜底，不再用核数预判（A 档已保证 ≥2 核）。

B 档 TTS 的 Capability 原因码为 `insufficient_ram`（**已被「修订三」改为 `tts_ram_tier`**），用户说明"服务端朗读需要至少 4GB 内存，已使用系统语音"。最低配置表述：语音输入 2GB/2 核；服务端朗读 4GB（≥3600MB）/2 核 + 基准通过。

## 2026-10-08 修订三：分档提示与下载前代理测速

### 问题 1：前端不区分档位

B 档 TTS 沿用 `insufficient_ram` 与 A 档同码，前端无法区分"语音输入可用、只是服务端朗读关闭"和"全部不可用"，用户设为"仅服务端朗读"时 B 档会被 `unsupported` toast 打扰。

- 新增原因码 `tts_ram_tier`，**仅 B 档的 TTS** 使用（STT 支持、TTS 因总内存 <3600MB 关闭）；A 档仍为 `insufficient_ram` / `insufficient_cores`。
- 前端：服务端朗读 `unsupported`（任何原因）时，系统语音可用则直接朗读、**不弹任何 toast**；仅系统里也没有本地中文语音时才提示。原因文案只出现在状态 chip 下拉里（新增 `info` 级别，静态、不转圈），全部走 i18n：

| reason | 文案 |
|---|---|
| `tts_ram_tier` | 朗读使用系统语音（服务端朗读需 4GB 内存）；语音输入可用 |
| `insufficient_ram` / `insufficient_cores` | 本机配置不足（需 2GB 内存、2 核），语音输入与服务端朗读不可用 |
| `too_slow` | 本机 CPU 合成速度不足，朗读使用系统语音 |

### 问题 2：弱 CPU 白下 167MB

下载前用已装好的 SenseVoice 对固定噪声测速并外推 Melo RTF，过慢则不下载。

**测量**：10s、16kHz、幅度 0.01、固定种子噪声（不依赖资产文件）；1 次预热 + 3 次计时取最小；线程数 = Melo 线程数 `min(4, 逻辑核)`；用临时 STT 引擎，测完释放，不扰动 Slot 常驻引擎。

**标定（Go 管线，2026-10-08，取代下方作废的 Python 标定）**：系数必须来自 Go 管线（生产同一份 sherpa-onnx 1.13.2 + ORT 1.24.4）同一时刻的测量。方法：Apple M1（8 核/16GB），`proxy_calibrate_integration_test.go` 对 t=1/2/4 交替各跑 3 轮 Go `MeasureSTTRTF`（SenseVoice，10s 固定噪声，1 预热 + 3 计时取最小）与 Go Melo 真实基准（`RunBench`：ADR-0108 基准句，预热 1 + 计时 2 取最小），ratio = meloRTF / sttRTF：

| 线程 | 轮次 | sttRTF | meloRTF | ratio |
|---|---|---|---|---|
| 1 | 1 | 0.1584 | 0.8769 | 5.54 |
| 1 | 2 | 0.1444 | 1.1741 | 8.13 |
| 1 | 3 | 0.2064 | 1.0615 | 5.14 |
| 2 | 1 | 0.1344 | 0.7864 | 5.85 |
| 2 | 2 | 0.1174 | 0.6619 | 5.64 |
| 2 | 3 | 0.1100 | 0.5433 | 4.94 |
| 4 | 1 | 0.0796 | 0.4117 | 5.17 |
| 4 | 2 | 0.0786 | 0.3804 | 4.84 |
| 4 | 3 | 0.0779 | 0.3959 | 5.08 |

全部轮次 ratio 最小值 4.84，**系数 = 4.84 × 0.9 ≈ 4.35**（低估 Melo 耗时：只会多下载，不会误杀）。

<details><summary>已作废：Opus 的 Python 标定（系数 13）</summary>

Python 管线（sherpa_onnx 1.13.8），与 Go 管线不可比，**已作废**：SenseVoice 1/2/4 线程 RTF 0.0391/0.0223/0.0172，Melo 0.79/0.42/0.233，比值 20/19/13.5。

</details>

**为什么两条管线不可比（Go STT 慢 3 倍的根因）**：同机同输入背靠背对比，Python 1.13.8 的 SenseVoice RTF 1/2/4 线程最优 0.0385/0.0231/0.0232，Go 管线 0.136/0.075/0.076。逐项排除：FFI 的 `num_threads`/`debug`/`provider` 偏移正确（线程数确实生效，RTF 随线程下降）；计时只含 Transcribe，标点模型对噪声输入不触发（输出为空）；噪声分布（均匀/高斯）与是否加载标点模型无差别；每次计时不重建 recognizer。决定性实验：同一份 Go FFI 代码改加载 pip 的 1.13.8 动态库，RTF 变为 0.040/0.0143，与 Python 完全一致；仅把 1.13.2 的 c-api 配上 pip 自带的新 ORT（1.28.2），1 线程即达 0.040、4 线程 0.022。**结论：慢的是 1.13.2 发行包捆绑的 ORT 1.24.4 在 macOS arm64 上的推理性能，不是 Go 路径缺陷**；Melo 同样受影响（Go 4 线程 ≈0.40 vs Python 0.233）。升级 sherpa/ORT 会牵动 ABI 钉死（偏移需重新 clang 实测）与五个平台的清单 sha256，不在本修订范围，列为后续事项（见重新评估触发条件）。

**公式**：`predicted = sttRTF × 4.35`；`predicted > 1.0` 判代理过慢（对应 sttRTF > 0.2299）。1.0 比真实基准的 0.8 宽松，因为代理有误差，边缘机型交给真实基准定夺。

**流程**：
- 代理过慢：不下载 Melo，TTS 置 `unsupported/too_slow`，落库 `audio.tts_bench.melo`（`method:"proxy"`、`stt_rtf`、`predicted`，`rtf` 存 predicted），后台预置器尊重该结论，重启后同指纹直接生效。
- 否则下载并走原真实基准（ADR-0108 D2）。
- 复用 D2 的 CPU 空闲采样与 Contended 判定：测前/测后 CPU ≥50% 视为争用，**无结论**，走原路径（下载 + 真实基准），不落库。
- STT 资产缺失、内存不足或加载失败：跳过代理，走原路径。
- **逃生口**：用户主动 `POST /v1/audio/tts/install` 无视代理结论，清除代理 slow、下载并跑真实基准，真实结果覆盖代理记录（代理记录不算"已重测"，真实基准的首个慢结果仍享有一次重测）。真实基准的定论过慢仍拒绝安装（已测过且资产已回收）。
- **回收**：真实基准定论过慢（非争用、已重测仍慢、`RetryOnStart=false`）后删除 `models/tts/melo/`，状态保持 `unsupported/too_slow`。争用造成的暂时不可用绝不删除。启动恢复（`restoreBench`）读到同指纹的真实定论 too_slow（method 非 proxy、`RetryOnStart=false`）而 `models/tts/melo/` 仍在时，同样补回收；代理记录从未下载过 Melo，不处理。

**并发与资源安全**：回收发生在 Slot 加载回调内、引擎已 `Close` 之后，此刻无驻留引擎与 inflight；`slow` 先于删除置位，`gate()` 与 `loadEngine` 入口均据此拒绝后续加载，不会再触碰该目录；`assetMu` 使"下载/解压"与"回收"互斥；安装本身由 `installing` CAS 保证预置器与手动安装互斥；回收只删目录名恰为 `melo` 的确切路径。

**可观测（HE-1）**：slog 记录 stt_rtf / predicted / contended / 回收；计数器 `polaris.audio.tts_proxy_bench_total`、`tts_proxy_too_slow_total`、`tts_proxy_skipped_total`、`tts_reclaim_total`。

**M1 自检**：新系数下 M1 实测 sttRTF 1/2/4 线程 0.1435/0.0743/0.0847，predicted 0.62/0.32/0.37，2 与 4 线程均放行（M1 真实 Go Melo：2 线程 0.54–0.79、4 线程 0.38–0.41，其中 2 线程贴近 0.8 线，弱核机器会由真实基准定论）。

## 决策

1. **模型**（Matcha 部分已被 2026-10-08 修订取代）：服务端 TTS 删除 Kokoro，改为两档：
   - **标准档 = MeloTTS zh_en fp32**（`tts-models/vits-melo-tts-zh_en.tar.bz2`，sherpa VITS 配置，需 `dict_dir` 与 `lexicon`，rule_fsts=`phone.fst,date.fst,number.fst,new_heteronym.fst`）。归档内 `model.int8.onnx` 为 133 字节占位文件，不使用、不列入必需文件。
   - **轻量档 = Matcha zh-en**（已被 2026-10-08 修订取代）（`tts-models/matcha-icefall-zh-en.tar.bz2` + `vocoder-models/vocos-16khz-univ.onnx` 单文件，sherpa Matcha 配置，rule_fsts=`phone-zh.fst,date-zh.fst,number-zh.fst`）。
   - 二者均单说话人，`sid=0`；删除配置键 `kokoro_sid`（旧键仅 Warn，按 ADR-0107 决策 9 模式）。
2. **硬件三档**（已被 2026-10-08 修订取代为两档；只看稳定画像：总内存 + 逻辑核数；阈值沿用 ADR-0107 的 88% 余量口径）：

   | 档 | 条件 | STT（SenseVoice） | 服务端 TTS | 后台预置（auto_install） |
   |---|---|---|---|---|
   | A 极低配 | 总内存 < 1800MB，或逻辑核 < 2 | 关 | 关，前端系统语音 | 不预置任何语音资产 |
   | B 低配 | 不属 A，且（总内存 < 3600MB 或逻辑核 < 4） | 开 | Matcha | STT → Matcha |
   | C 标准 | 总内存 ≥ 3600MB 且逻辑核 ≥ 4 | 开 | MeloTTS | STT → MeloTTS |

   典型机型：1GB 或单核 VPS → A；2GB/2 核 VPS、4GB 双核旧笔记本 → B；4GB/4 核及以上桌面 → C。
   理由：Matcha 单线程 RTF 0.063、单句 RSS 280MB，2GB 机器在空闲内存门槛保护下可承受；MeloTTS 单句 530MB、2 线程 RTF 0.42，弱核上贴近 0.8 门槛，只放给 C 档。单核机器即使内存充足也关闭：推理会与核心路径抢唯一的核。
3. **配置**（已被 2026-10-08 修订取代：`inference.tts.model` 已删除）：`inference.tts.model = "auto" | "melo" | "matcha"`，默认 `auto`（按上表选档）。显式指定可越档（B 档机器指定 `melo` 允许，仍受空闲内存门槛与基准门控约束），但不能解锁 A 档。
4. **空闲内存门槛（只决定此刻能否加载；Matcha 450MB 已被 2026-10-08 修订取代）**：STT 600MB（不变）、MeloTTS 800MB、Matcha 450MB（单句 RSS × ≈1.5）。
5. **基准与降级链**（降级链 Melo→Matcha 已被 2026-10-08 修订取代：Melo too_slow 直接回系统语音）：两档沿用 ADR-0108 D2 抗争用基准（RTF ≤ 0.8）。基准结果持久化时指纹须含模型名，旧 Kokoro 结论不得复用。MeloTTS 定论 too_slow（非争用、重测仍超标）→ 自动降到 Matcha（需要时安装 Matcha 资产）→ Matcha 也 too_slow → 前端系统语音。降级链每步都在 `/v1/audio/status` 可见（当前模型名 + 原因码）。
6. **合成参数**：`max_num_sentences = 100`（禁止子句级拆批，见上下文吞字实测）；服务端在 `Generate` 内按句末标点（`。！？!?；;` 与换行）切句、逐句合成、拼接 PCM，单次峰值内存只取决于最长一句；`silence_scale = 0.2` 不变。线程：MeloTTS `min(4, 逻辑核)`，Matcha `min(2, 逻辑核)`（Matcha 部分已被 2026-10-08 修订取代）。
7. **旧资产清理**（修订后另删 `models/tts/matcha/`）：新 TTS 资产安装并通过校验后，删除 polaris 管理的旧目录 `models/kokoro/`（≈330MB），只删该目录，失败仅 Warn。
8. **FFI**（Matcha 子配置已被 2026-10-08 修订取代，仅保留 VITS）：MeloTTS（VITS）与 Matcha 子配置在 `SherpaOnnxOfflineTtsModelConfig` 中的偏移必须按 1.13.2 `c-api.h` 用 clang `offsetof` 实测后手写，测量方法与结果写入 `internal/llm/tts/sherpa.go` 注释；不升级 sherpa 版本（ABI 钉死 1.13.2 不变）。

## 后果

- **正向**: 中文朗读真人感显著提升；2GB VPS 从"无服务端朗读"升为 Matcha；资产体积 C 档 167MB、B 档 133MB，均小于 Kokoro 330MB；整句合成消除吞字。
- **负向**: MeloTTS 词典外英文长词逐字母拼读；输出 44.1kHz WAV 体积约为 Kokoro 24kHz 的 1.8 倍；多一条降级路径与一个单文件资产类型。~~Matcha 模型来自 modelscope 第三方上传，许可证未在归档中声明，分发前需用户确认。~~ 该风险已通过删除 Matcha 消除（2026-10-08 修订）。
- **反例守护**: 拒绝 `max_num_sentences=1` 跑 MeloTTS（吞字实测）；拒绝在 A 档开启任何服务端语音或后台预置；拒绝把一次性长文本整段送入 MeloTTS（峰值内存随长度增长）；拒绝复用不含模型名的基准指纹；拒绝重新引入 Kokoro（用户试听淘汰）；拒绝引入训练数据来源不可核验或含非商业条款的权重（Matcha zh-en 实例）。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 保留 Kokoro | 用户试听中文音质最差 |
| ZipVoice-distill int8 | RTF 0.34、需参考音频，听感不及 MeloTTS/Matcha |
| macOS 系统语音作默认 | 精简版音质差，高级版需用户手动下载，且仅 Mac；保留为前端兜底 |
| FFmpeg 后处理提升音质 | 只能改音色不能改韵律，且引入数十 MB 依赖与许可问题 |
| 2GB VPS 一律关闭 TTS | 已被 2026-10-08 修订部分采纳：Melo 在 2GB/2 核上由空闲内存门槛与基准门控把关，不再有 Matcha 中间档 |
| Matcha zh-en + vocos-16k 作轻量档（2026-10-08 修订追加） | 训练数据源自标贝 CSMSC（非商业条款），Apache-2.0 声明与上游冲突，链路不可核验；归档附带 GPL-3.0 的 espeak-ng-data；开源自托管项目不得默认下发 |

## 引用代码

- `internal/llm/audiorun/support.go`（三档判定）
- `internal/llm/audioassets/manifest.go`（资产清单）
- `internal/llm/tts/sherpa.go`（FFI 配置与分句合成）
- `cmd/polaris/server_stt_tts.go`（装配与降级链）

## 重新评估触发条件

0. 代理测速在空闲机器上的 predicted 与真实 Melo RTF 偏差持续超过 2 倍（见修订三「已知局限」），需重新标定系数 4.35；升级 sherpa-onnx/ORT（见修订三根因）后必须重新标定。
1. sherpa-onnx 上游修复子句拆批吞字，或 MeloTTS 提供英文 G2P 回退（可恢复长英文词）。
2. 出现 RTF 与音质均优于 MeloTTS 且单句 RSS ≤ 600MB 的中文模型。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-10-08 | 初稿 |
| 2026-10-08 | 修订：删除 Matcha（许可证链路不可核验），档位收敛为两档，删除 `inference.tts.model` 与降级链 |
| 2026-10-08 | 修订二：2GB 档（B）关闭服务端 TTS 只开 STT，档位改为 A/B/C 三档，TTS 总内存门槛 3600MB、核数门槛 2 |
| 2026-10-08 | 修订三：新增原因码 `tts_ram_tier` 与前端分档提示（系统语音可用时不弹 toast）；下载前 SenseVoice 代理测速（predicted=sttRTF×4.35，>1.0 不下载；系数由 Go 管线同时刻标定）、手动安装逃生口、真实定论过慢后回收 `models/tts/melo`（含启动时补回收） |
