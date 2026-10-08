# ADR-0110: 语音 v4：MeloTTS / Matcha 取代 Kokoro，按硬件三档开放语音能力

- **状态**: Accepted
- **日期**: 2026-10-08
- **决策者**: 用户（试听裁决）/ Opus（实测与设计）/ Sonnet（实现）
- **相关模块**: M13 Gateway / `internal/llm/audioassets/` / `internal/llm/audiorun/` / `internal/llm/tts/` / `internal/config/` / `cmd/polaris/server_stt_tts.go` / `web/src/js/store/`
- **取代**: ADR-0107 决策 1 的 TTS 部分（Kokoro v1.1 fp32）、决策 2 的服务端 TTS 门槛、决策 6（Kokoro 文本规整）、反例守护中"拒绝重新引入 Matcha"；ADR-0108 D3 中"Kokoro 就绪"的表述。ADR-0107 的 STT 选型、懒加载/空闲卸载、错误契约，ADR-0108 的串行预置、抗争用基准、前端单一进度指示继续有效。

## 上下文

用户反馈 Kokoro 朗读中文音色差。2026-10-04 在 Mac mini M1（8 核/16GB，sherpa-onnx 1.13.8 Python 绑定，与 1.13.2 C API 同模型格式）上用同一段中英混排文本盲听 5 个方案：macOS 婷婷（精简版）、Kokoro v1.1 zf_001、ZipVoice-distill int8、MeloTTS zh_en、Matcha zh-en。**用户裁决：MeloTTS 真人感最强，Matcha 次之，二者音质最好**。

新事实（推翻 ADR-0107 "拒绝 Matcha" 的依据）：ADR-0107 实测的是旧版 Matcha + vocos-22k，英文词含糊（GitHub→"g hop"）。本次为 `matcha-icefall-zh-en`（上游 modelscope `dengcunqin/matcha_tts_zh_en_20251010`）+ `vocos-16khz-univ`，SenseVoice 回读：`DOCKER`、`MAKE LINT`、`REQUEST`、`THIS MODEL ... BOTH ENGLISH AND CHINESE` 均正确，长技术词（Kubernetes/GitHub）略含糊。

实测（M1，SenseVoice 回读，RSS 为进程峰值减去 Python 基线 18MB）：

| 模型 | 下载体积 | RTF 1/2/4 线程 | 单句 RSS | 20s 长段一次合成 RSS | 英文 | 用户听感 |
|---|---|---|---|---|---|---|
| MeloTTS zh_en fp32（44.1kHz） | 167MB | 0.79 / 0.42 / 0.23 | ≈530MB | ≈810MB | 短词好；词典外长词逐字母拼读（Kubernetes→K U B…） | 第 1 |
| Matcha zh-en + vocos-16k（16kHz） | 79MB + 54MB | 0.063 / 0.035–0.040 / - | ≈280MB | ≈345MB | 可懂 | 第 2 |
| Kokoro v1.1 fp32（现状） | 330MB | - / - / - | ≈600MB（ADR-0107） | - | 最好 | 差 |

**MeloTTS 致命缺陷与规避**：sherpa-onnx 按标点（含逗号）切分子句，`max_num_sentences=1` 时每个子句单独合成，短子句会被吞字——"我说另外，请打开。"回读为"你拿开"，"另外，…"句首"另外"丢失。`max_num_sentences=100`（整句同批）后回读完全正确。代价是长文本一次合成峰值内存随长度增长（上表 810MB），故服务端须按句末标点自行分句、逐句合成后拼接。

## 决策

1. **模型**：服务端 TTS 删除 Kokoro，改为两档：
   - **标准档 = MeloTTS zh_en fp32**（`tts-models/vits-melo-tts-zh_en.tar.bz2`，sherpa VITS 配置，需 `dict_dir` 与 `lexicon`，rule_fsts=`phone.fst,date.fst,number.fst,new_heteronym.fst`）。归档内 `model.int8.onnx` 为 133 字节占位文件，不使用、不列入必需文件。
   - **轻量档 = Matcha zh-en**（`tts-models/matcha-icefall-zh-en.tar.bz2` + `vocoder-models/vocos-16khz-univ.onnx` 单文件，sherpa Matcha 配置，rule_fsts=`phone-zh.fst,date-zh.fst,number-zh.fst`）。
   - 二者均单说话人，`sid=0`；删除配置键 `kokoro_sid`（旧键仅 Warn，按 ADR-0107 决策 9 模式）。
2. **硬件三档**（只看稳定画像：总内存 + 逻辑核数；阈值沿用 ADR-0107 的 88% 余量口径）：

   | 档 | 条件 | STT（SenseVoice） | 服务端 TTS | 后台预置（auto_install） |
   |---|---|---|---|---|
   | A 极低配 | 总内存 < 1800MB，或逻辑核 < 2 | 关 | 关，前端系统语音 | 不预置任何语音资产 |
   | B 低配 | 不属 A，且（总内存 < 3600MB 或逻辑核 < 4） | 开 | Matcha | STT → Matcha |
   | C 标准 | 总内存 ≥ 3600MB 且逻辑核 ≥ 4 | 开 | MeloTTS | STT → MeloTTS |

   典型机型：1GB 或单核 VPS → A；2GB/2 核 VPS、4GB 双核旧笔记本 → B；4GB/4 核及以上桌面 → C。
   理由：Matcha 单线程 RTF 0.063、单句 RSS 280MB，2GB 机器在空闲内存门槛保护下可承受；MeloTTS 单句 530MB、2 线程 RTF 0.42，弱核上贴近 0.8 门槛，只放给 C 档。单核机器即使内存充足也关闭：推理会与核心路径抢唯一的核。
3. **配置**：`inference.tts.model = "auto" | "melo" | "matcha"`，默认 `auto`（按上表选档）。显式指定可越档（B 档机器指定 `melo` 允许，仍受空闲内存门槛与基准门控约束），但不能解锁 A 档。
4. **空闲内存门槛（只决定此刻能否加载）**：STT 600MB（不变）、MeloTTS 800MB、Matcha 450MB（单句 RSS × ≈1.5）。
5. **基准与降级链**：两档沿用 ADR-0108 D2 抗争用基准（RTF ≤ 0.8）。基准结果持久化时指纹须含模型名，旧 Kokoro 结论不得复用。MeloTTS 定论 too_slow（非争用、重测仍超标）→ 自动降到 Matcha（需要时安装 Matcha 资产）→ Matcha 也 too_slow → 前端系统语音。降级链每步都在 `/v1/audio/status` 可见（当前模型名 + 原因码）。
6. **合成参数**：`max_num_sentences = 100`（禁止子句级拆批，见上下文吞字实测）；服务端在 `Generate` 内按句末标点（`。！？!?；;` 与换行）切句、逐句合成、拼接 PCM，单次峰值内存只取决于最长一句；`silence_scale = 0.2` 不变。线程：MeloTTS `min(4, 逻辑核)`，Matcha `min(2, 逻辑核)`。
7. **旧资产清理**：新 TTS 资产安装并通过校验后，删除 polaris 管理的旧目录 `models/kokoro/`（≈330MB），只删该目录，失败仅 Warn。
8. **FFI**：MeloTTS（VITS）与 Matcha 子配置在 `SherpaOnnxOfflineTtsModelConfig` 中的偏移必须按 1.13.2 `c-api.h` 用 clang `offsetof` 实测后手写，测量方法与结果写入 `internal/llm/tts/sherpa.go` 注释；不升级 sherpa 版本（ABI 钉死 1.13.2 不变）。

## 后果

- **正向**: 中文朗读真人感显著提升；2GB VPS 从"无服务端朗读"升为 Matcha；资产体积 C 档 167MB、B 档 133MB，均小于 Kokoro 330MB；整句合成消除吞字。
- **负向**: MeloTTS 词典外英文长词逐字母拼读；输出 44.1kHz WAV 体积约为 Kokoro 24kHz 的 1.8 倍；多一条降级路径与一个单文件资产类型。Matcha 模型来自 modelscope 第三方上传，**许可证未在归档中声明，分发前需用户确认**。
- **反例守护**: 拒绝 `max_num_sentences=1` 跑 MeloTTS（吞字实测）；拒绝在 A 档开启任何服务端语音或后台预置；拒绝把一次性长文本整段送入 MeloTTS（峰值内存随长度增长）；拒绝复用不含模型名的基准指纹；拒绝重新引入 Kokoro（用户试听淘汰）。

## 被驳回的方案

| 方案 | 驳回理由 |
|------|---------|
| 保留 Kokoro | 用户试听中文音质最差 |
| ZipVoice-distill int8 | RTF 0.34、需参考音频，听感不及 MeloTTS/Matcha |
| macOS 系统语音作默认 | 精简版音质差，高级版需用户手动下载，且仅 Mac；保留为前端兜底 |
| FFmpeg 后处理提升音质 | 只能改音色不能改韵律，且引入数十 MB 依赖与许可问题 |
| 2GB VPS 一律关闭 TTS | Matcha 单线程 RTF 0.063 / 280MB，有空闲内存门槛保护即可安全开启 |

## 引用代码

- `internal/llm/audiorun/support.go`（三档判定）
- `internal/llm/audioassets/manifest.go`（资产清单）
- `internal/llm/tts/sherpa.go`（FFI 配置与分句合成）
- `cmd/polaris/server_stt_tts.go`（装配与降级链）

## 重新评估触发条件

1. sherpa-onnx 上游修复子句拆批吞字，或 MeloTTS 提供英文 G2P 回退（可恢复长英文词）。
2. Matcha 许可证确认不可再分发。
3. 出现 RTF 与音质均优于 MeloTTS 且单句 RSS ≤ 600MB 的中文模型。

## 修订记录

| 日期 | 变更 |
|------|------|
| 2026-10-08 | 初稿 |
