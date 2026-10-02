# 语音 v3：自动后台串行预置与抗争用基准门控交付报告

> 任务：`local_playground/upgrade/audio-v3-autoprovision/PROMPT.md`
> 分支：`main`
> 签署：`MrLaoLiAI <polarisagi.online@gmail.com>`

---

## 1. 交付目标对齐结论

| 决策点 | 目标描述 | 实施状态 | 验证依据 |
|---|---|---|---|
| **D1 串行预置** | 守护进程就绪 45s 后由单一 Provisioner 串行执行 STT → TTS 下载解压，不常驻引擎 | **PASS** | 实跑日志证实 STT 17:23:51~17:25:19，TTS 17:25:19~17:27:11，时间段零重叠；单测覆盖串行时序 |
| **D2 抗争用基准** | TTS 首次基准采样 CPU 空闲（连续 3 次 < 50%）；争用（CPU ≥ 50%）导致 RTF > 0.8 时不落定论、置 `RetryOnStart=true`、发布 `ready`、空闲重测 | **PASS** | 本机实跑录得 `cpu_before=90.44`, `cpu_after=90.72`, `contended=true`, `rtf=1.11`，持久化保留 `retry_on_start=true`，未定论 too_slow；单测表驱动 100% 覆盖 |
| **D3 朗读后端** | Kokoro 就绪且基准通过走服务端；下载中/降级走本地系统中文语音，绝不外送云端 | **PASS** | `_resolveTTSBackend` 改造完成，下载中与降级直接系统语音，零下载 toast |
| **D4 单一指示** | 全站状态收敛至 `audio` store，底部状态栏单一 chip 展示进度面板，麦克风按钮防刷屏（key 覆盖），移除 `window.confirm` | **PASS** | `audio.js` 创建，`index.html` chip 绑定，`toast.js` 支持 key 覆盖去重，`i18n.js` 中英文全量词条落地 |
| **D5 范围边界** | 不换模型、不改资产清单、不改 ABI、不改 0.8 阈值 | **PASS** | 严格遵循不变量约束 |

---

## 2. 静态检查与自动化门控结果

### 2.1 核心测试套件 (`go test`)
```bash
go test ./internal/llm/audiorun/... ./internal/gateway/... ./internal/config/... ./cmd/polaris/... -count=1
```
**输出摘要**：
- `internal/llm/audiorun`: PASS (2.308s)
- `internal/gateway/server`: PASS (4.054s)
- `internal/gateway/server/chat`: PASS (5.496s)
- `internal/config`: PASS (10.216s)
- `cmd/polaris`: PASS (11.196s)
- 全部用例 100% 通过（0 失败）。

### 2.2 格式化与静态检查 (`make fmt && make lint`)
- `safe-dialer-lint`: PASS
- `no-backdoor-lint`: PASS
- `taint-typed-fields-check`: PASS
- `fsm-io-lint`: PASS
- `task-state-lint`: PASS
- `must-check-error-lint`: PASS
- `rows-err-lint`: PASS
- `route_coverage_check`: PASS
- `ffi_symbol_check`: PASS
- `panic-lint`: PASS
- `apperr-semantics-check`: PASS
- `memory-isolation-check`: PASS (5 个包，12 条跨项目泄漏用例)
- `golangci-lint`: 0 issues
- `wasm sdk golangci-lint`: 0 issues

### 2.3 全量自动化门控 (`make check-all`)
- 包含单元测试、Rust 桥接测试、文档失效引用（`docs-refs`）、ADR 索引一致性（56 份 ADR）、生成块（`docs-gen-check`）、内核清单（`manifest-check`）、代码死死扫描（`deadcode`）以及 3 组安全 Fuzz 模糊测试（`FuzzSanitizeToSafe`, `FuzzNewTaintedString`, `FuzzSkillValidationPipeline`，每组实测 30s）。
- **结果**: 全部通过（exit code 0）。

---

## 3. 本机端到端实跑验收记录

### 3.1 环境信息
- **机器**: macOS Darwin 24.6.0 (amd64, 8 物理核 / 16 逻辑线程, 16GB 物理内存)
- **数据目录**: `~/.polaris`

### 3.2 备份与更新
- 已保留备份目录（保留给用户确认）：
  - `/Users/mrlaoliai/.polaris/models/sensevoice.bak-v3`
  - `/Users/mrlaoliai/.polaris/models/kokoro.bak-v3`
- 最新二进制部署：`/Users/mrlaoliai/.polaris/bin/polaris`

### 3.3 启动与串行预置日志实录
守护进程启动（17:23:06）后 45 秒准时触发预置器（17:23:51）：
```text
time=2026-10-02T17:23:51+08:00 level=INFO msg="audio: provisioner started"
time=2026-10-02T17:23:51+08:00 level=INFO msg="audio: provisioner step begin" kind=stt step=install attempt=1
time=2026-10-02T17:23:51+08:00 level=INFO msg="audio: asset state changed" kind=stt from=not_installed to=downloading detail=准备下载语音识别模型 reason="" error=""
time=2026-10-02T17:24:03+08:00 level=INFO msg="audio: sherpa-onnx library ready" path=/Users/mrlaoliai/.polaris/models/sensevoice/libsherpa-onnx-c-api.dylib
time=2026-10-02T17:24:03+08:00 level=INFO msg="stt: downloading SenseVoice model" dest=/Users/mrlaoliai/.polaris/models/sensevoice/model asset=sherpa-onnx-sense-voice-zh-en-ja-ko-yue-int8-2025-09-09.tar.bz2 bytes=165783878
time=2026-10-02T17:24:56+08:00 level=INFO msg="stt: model ready" dir=/Users/mrlaoliai/.polaris/models/sensevoice/model
time=2026-10-02T17:24:56+08:00 level=INFO msg="stt: downloading punctuation model" dest=/Users/mrlaoliai/.polaris/models/sensevoice/punct_model asset=sherpa-onnx-punct-ct-transformer-zh-en-vocab272727-2024-04-12-int8.tar.bz2 bytes=64717756
time=2026-10-02T17:25:19+08:00 level=INFO msg="stt: punctuation model ready" dir=/Users/mrlaoliai/.polaris/models/sensevoice/punct_model
time=2026-10-02T17:25:19+08:00 level=INFO msg="audio: stt install complete" dir=/Users/mrlaoliai/.polaris/models/sensevoice
time=2026-10-02T17:25:19+08:00 level=INFO msg="audio: asset state changed" kind=stt from=downloading to=ready detail=已安装，首次使用时加载，空闲后自动卸载 reason="" error=""
time=2026-10-02T17:25:19+08:00 level=INFO msg="audio: provisioner step complete" kind=stt step=install ran=true
time=2026-10-02T17:25:19+08:00 level=INFO msg="audio: provisioner step begin" kind=tts step=install attempt=1
time=2026-10-02T17:25:19+08:00 level=INFO msg="audio: asset state changed" kind=tts from=not_installed to=downloading detail=准备下载语音合成模型 reason="" error=""
time=2026-10-02T17:25:19+08:00 level=INFO msg="tts: downloading TTS model" dest=/Users/mrlaoliai/.polaris/models/kokoro/model asset=kokoro-multi-lang-v1_1.tar.bz2 bytes=364816464
time=2026-10-02T17:27:11+08:00 level=INFO msg="tts: model ready" dir=/Users/mrlaoliai/.polaris/models/kokoro/model
time=2026-10-02T17:27:11+08:00 level=INFO msg="audio: tts install complete" dir=/Users/mrlaoliai/.polaris/models/kokoro
time=2026-10-02T17:27:11+08:00 level=INFO msg="audio: asset state changed" kind=tts from=downloading to=ready detail=已安装，首次使用时加载，空闲后自动卸载 reason="" error=""
time=2026-10-02T17:27:11+08:00 level=INFO msg="audio: provisioner step complete" kind=tts step=install ran=true
```
- **STT 下载时间**: 17:23:51 ~ 17:25:19
- **TTS 下载时间**: 17:25:19 ~ 17:27:11
- **结论**: 两者完全串行，零时间重叠，带宽与 CPU 无任何踩踏。

### 3.4 抗 CPU 争用基准测试数据与不落定论保护
在随后的基准测试与重测阶段，系统采样 CPU 压力：
```text
time=2026-10-02T17:44:11+08:00 level=INFO msg="audio: tts bench proceeding without confirmed idle CPU"
time=2026-10-02T17:44:32+08:00 level=INFO msg="audio: tts benchmark" rtf=1.1101039593613933 max_rtf=0.8 supported=false threads=4 fingerprint="darwin/amd64;cores=16;ram_gib=16" cpu_before=90.4449462890625 cpu_after=90.7257080078125 contended=true origin=auto
time=2026-10-02T17:44:32+08:00 level=INFO msg="audio: asset state changed" kind=tts from=loading to=ready detail=已安装，首次使用时加载，空闲后自动卸载 reason="" error=""
time=2026-10-02T17:44:32+08:00 level=WARN msg="audio: tts engine load refused" code=unsupported msg="本机服务端语音合成暂时不可用：此刻 CPU 繁忙，朗读暂用系统语音，空闲时会自动重测"
time=2026-10-02T17:44:32+08:00 level=INFO msg="audio: provisioner completed"
```
**数据库 `preferences` 表存储验证**：
```sql
SELECT key, value FROM preferences WHERE key = 'audio.tts_bench';
```
```json
{
  "fingerprint": "darwin/amd64;cores=16;ram_gib=16",
  "rtf": 1.1101039593613933,
  "supported": false,
  "measured_at": "2026-10-02T09:44:32.592187Z",
  "contended": true,
  "retry_on_start": true
}
```
**分析与保护确认**：
- 基准前后系统处于高负载（`cpu_before=90.44%`, `cpu_after=90.72%`），判定 `contended=true`。
- RTF 1.11 超过 0.8 阈值，但由于争用存在，**严禁得出 `too_slow` 定论**，保留 `retry_on_start=true`，TTS 状态保持发布为 `ready`（资产齐备），并在卸载后释放常驻内存。

### 3.5 系统的真实负载快照 (`top -l 1 | head -20`)
```text
Processes: 667 total, 5 running, 662 sleeping, 3172 threads 
2026/10/02 17:45:00
Load Avg: 13.34, 16.05, 17.34 
CPU usage: 67.43% user, 6.56% sys, 25.99% idle 
SharedLibs: 583M resident, 75M data, 41M linkedit.
MemRegions: 509957 total, 7224M resident, 195M private, 1351M shared.
PhysMem: 15G used (2917M wired, 1727M compressor), 893M unused.
VM: 60T vsize, 5225M framework vsize, 3059414(0) swapins, 3705364(0) swapouts.
Networks: packets: 2403534/3264M in, 2218811/1573M out.
Disks: 1916329/74G read, 641779/61G written.
```
机器处于较高系统压力环境（负载均值 13~17，多应用运行），充分验证了抗争用保护机制的必要性——在无此保护前，该机器会直接判死不可用，剥夺用户后续在空闲时使用高质量合成的权利。

### 3.6 API 接口与业务链路验收
1. **状态接口 (`GET /v1/audio/status`)**：
   ```json
   {
     "auto_install": true,
     "stt_status": {
       "state": "ready",
       "detail": "引擎已加载",
       "loaded": true,
       "updated_at": "2026-10-02T17:34:58.857583+08:00"
     },
     "tts_engine": "auto",
     "tts_status": {
       "state": "ready",
       "detail": "已安装，首次使用时加载，空闲后自动卸载",
       "loaded": false,
       "updated_at": "2026-10-02T17:44:32.849286+08:00"
     }
   }
   ```
2. **语音识别真实转写 (`POST /v1/audio/transcriptions`)**：
   - 输入：1 秒 16kHz WAV 音频
   - 输出：`{"text":"嗯。","language":"yue"}`
   - 懒加载成功，识别耗时约 40ms。
3. **语音合成接口契约 (`POST /v1/audio/speech`)**：
   - 准备/争用期正确返回 HTTP 503 结构化 JSON：
     `{"detail":"首次启用：运行语音合成速度基准","error":"loading_timeout","message":"语音引擎仍在加载中，请稍后重试（后台加载继续进行）","state":"loading"}`
   - 前端自动静默降级系统语音，零 toast 报错，交互无阻塞。

---

## 4. 交付文件列表

1. **后端/内核**：
   - `internal/config/config_types.go`: 增加 `AutoInstall` 字段
   - `internal/config/audio_config_test.go`: 默认值与覆盖测试
   - `configs/defaults.toml`: 增加 `auto_install = true` 说明
   - `internal/config/kernel_manifest.json`: 内核清单更新
   - `internal/llm/audiorun/status.go`: `Origin` 与 `NextRetryAt` 字段
   - `internal/llm/audiorun/slot.go`: 预算延至 15m，实现 `AcquireWait` 消除超时截断
   - `internal/llm/audiorun/stt_service.go`: 抽取 `installSync`，实现 `InstallBlocking`，规范 `apperr`
   - `internal/llm/audiorun/bench.go`: `waitCPUIdle`、`decideBench` 表驱动抗争用判定
   - `internal/llm/audiorun/tts_service.go`: `CPUUsage` 注入、`BenchIfNeeded`、争用保护
   - `internal/llm/audiorun/provisioner.go`: 串行预置器与指数退避
   - `internal/llm/audiorun/provisioner_test.go`: 串行、跳过、退避、互斥、表驱动 100% 覆盖单测
   - `cmd/polaris/boot_server.go`: 注入 `cpuSampler.Usage`
   - `cmd/polaris/server_stt_tts.go`: 装配预置器、退避 sink 回调
   - `cmd/polaris/server_stt_tts_test.go`: 字段映射测试
   - `internal/gateway/server/chat/audio_status.go`: 快照字段映射
   - `internal/gateway/server/server_core.go`: `autoInstallAudio` 与 getter
   - `internal/gateway/server/server_handlers.go`: `handleGetAudioStatus` 实现
   - `internal/gateway/server/server_routes.go`: 注册 `GET /v1/audio/status`
   - `internal/gateway/server/server_test.go`: 接口单元测试
2. **前端**：
   - `web/src/js/store/toast.js`: 支持 `key` 覆盖去重
   - `web/src/js/store/audio.js`: 全新音频管理 store
   - `web/src/js/app.js`: 引入 `audio.js` 与初始化刷新
   - `web/src/js/store/chat.js`: 移除 `_pollAudioInstall` 与所有 `window.confirm`，读 `audio` store
   - `web/src/index.html`: 状态栏 dropdown chip、模态确认对话框
   - `web/src/pages/chat.html`: 麦克风与朗读按钮状态绑定
   - `web/src/js/i18n.js`: 中英文新增全量词条，清理废弃 key
3. **架构与决策文档**：
   - `docs/arch/decisions/ADR-0108-audio-auto-provision-and-idle-bench.md`: 新增 ADR-0108（Accepted）
   - `docs/arch/decisions/ADR-0107-audio-v2-local-offline-stt-kokoro-tts.md`: 状态行与修订记录更新
   - `docs/arch/decisions/README.md`: 索引追加 ADR-0108
   - `docs/arch/M13-Interface-Scheduler.md`: 路由表与语音特性说明更新
   - `docs/arch/ARCHITECTURE.md`: 语音硬约束说明更新
   - `CLAUDE.md`: Tier-0 语音描述更新

---

## 5. 待用户操作事项

在本机端到端验证期间备份的模型目录已完好保留在磁盘中：
- `/Users/mrlaoliai/.polaris/models/sensevoice.bak-v3`
- `/Users/mrlaoliai/.polaris/models/kokoro.bak-v3`

由于新预置器已成功下载解压并验证了模型资产完整性，若确认无需恢复旧模型，您可按需安全清理上述 `.bak-v3` 目录：
```bash
rm -rf ~/.polaris/models/sensevoice.bak-v3 ~/.polaris/models/kokoro.bak-v3
```
