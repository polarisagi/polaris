# 审核报告（代码轨道 · 批次 2）

| ID | 严重级 | 模块或对象 | 一句话标题 | 置信度 | 可机械化 |
|---|---|---|---|---|---|
| GR-2-001 | P1 | internal/llm | circuitBreaker 在 HalfOpen 探测失败时未恢复到 circuitOpen 状态，导致熔断器探活失败后卡死在全量放行状态 | 高 | 否 |
| GR-2-002 | P2 | internal/security | SafeDialer.dnsCache 缺乏容量上限与淘汰机制，长时运行下存在无界内存泄露风险 | 高 | 否 |
| GR-2-003 | P2 | internal/security | security/provider.go 声明的 AuditRepo / KillSwitchMetrics / GuardProvider 接口全仓零消费方 | 高 | 是 |

置信度分布声明: 本批次 3 条发现均基于直接代码逻辑推演与 §2-A 强制反证，全为高置信度。

---

### [GR-2-001] circuitBreaker 在 HalfOpen 探测失败时未恢复到 circuitOpen 状态，导致熔断器探活失败后卡死在全量放行状态
- 严重级: P1
- 模块: internal/llm（层: L0）
- 位置: internal/llm/circuit_breaker.go:70
- 违反规则: A-10
- 置信度: 高
- 可机械化: 否
- 反证: 已查 internal/llm/circuit_breaker.go (45-77 行) 与 provider_registry.go (180-220 行)，circuitBreaker 的 RecordFailure 为唯一的失败记录入口。当 state 为 circuitHalfOpen 时，RecordFailure() 调用 cb.failures.Add(1)，但 n >= maxFailures (默认为 5) 校验失败，导致 state 保持在 circuitHalfOpen，未重新置为 circuitOpen 且未更新 openUntil 冷却到期时间。已查 cmd/polaris/ 与 internal/llm/ 均无其他机制修正 HalfOpen 状态下的失败探测。
- 问题: circuitBreaker.RecordFailure() 在熔断器处于 circuitHalfOpen 探测状态下如果探活请求失败，未立即重新切回 circuitOpen 并刷新 openUntil 冷却期，而是仅递增 failures (从 0 变 1)。因为 1 < maxFailures (5)，state 保持在 circuitHalfOpen。这导致 Allow() 对后续所有并发请求均在 case circuitHalfOpen: 分支直接返回 true，使熔断器在探活失败后反而陷入放行全量请求的卡死状态，违反了 docs/specs/09-LLM-Agent-Production.md A-10 的熔断器状态机防护规则。
- 证据: internal/llm/circuit_breaker.go:70-76
  ```go
  func (cb *circuitBreaker) RecordFailure() {
  	n := cb.failures.Add(1)
  	if n >= cb.maxFailures {
  		cb.state.Store(int32(circuitOpen))
  		cb.openUntil.Store(time.Now().Add(cb.openDur).UnixNano())
  		cb.failures.Store(0)
  	}
  }
  ```
- 修复方向提示: 在 RecordFailure() 中判断若当前状态为 circuitHalfOpen，无条件重置 state 为 circuitOpen 并更新 openUntil 冷却到期时间。

### [GR-2-002] SafeDialer.dnsCache 缺乏容量上限与淘汰机制，长时运行下存在无界内存泄露风险
- 严重级: P2
- 模块: internal/security（层: L0）
- 位置: internal/security/network/safe_dialer.go:353
- 违反规则: A-06
- 置信度: 高
- 可机械化: 否
- 反证: 已查 internal/security/network/safe_dialer.go 第 50-54 行与 327-360 行、cmd/polaris/boot_substrate.go 及 internal/bootstrap/。SafeDialer 在 boot 时作为全局单例创建，其 dnsCache 与 dnsCacheTs map 仅在 resolveDNSBypass 中向 map 写入解析结果，全仓没有任何针对 dnsCache 的 delete、清理逻辑或 FIFO/LRU 容量上限控制。
- 问题: SafeDialer 中的 dnsCache 与 dnsCacheTs map 存储域名解析结果，虽然在 resolveDNS 中判断了 TTL 超时，但仅在未超时时复用缓存，超时后再次调用 resolveDNSBypass 重新写入 map。全仓没有任何清理旧 key 或容量限制的代码。当系统长期运行并请求不同域名时，dnsCache 中的 key 数量无上限增长，违反了 docs/specs/09-LLM-Agent-Production.md P-5 / A-06 缓存必须有容量上限与过期淘汰的要求。
- 证据: internal/security/network/safe_dialer.go:353-357
  ```go
  	// 写回缓存（更新时间戳）
  	sd.dnsCacheMu.Lock()
  	sd.dnsCache[host] = result
  	sd.dnsCacheTs[host] = time.Now()
  	sd.dnsCacheMu.Unlock()
  ```
- 修复方向提示: 为 SafeDialer.dnsCache 引入容量上限（如 LRU/FIFO map）或在写回时清理过期条目。

### [GR-2-003] security/provider.go 声明的 AuditRepo / KillSwitchMetrics / GuardProvider 接口全仓零消费方
- 严重级: P2
- 模块: internal/security（层: L0）
- 位置: internal/security/provider.go:27
- 违反规则: 维度G-bis-接线断裂
- 置信度: 高
- 可机械化: 是（建议规则: grep -rn "security\.AuditRepo" internal/ 且测试不计入）
- 反证: 已查 cmd/polaris/boot_*.go、internal/bootstrap/、internal/security/ 及全仓 internal/ 代码。AuditRepo、KillSwitchMetrics、GuardProvider 这三个导出接口仅在 internal/security/provider.go 中定义，在全仓生产代码及测试中均没有任何类型实现或变量引用。audit_trail.go 实际使用了 protocol.AuditRepository，killswitch.go 实际使用了 StateChangeCallback。
- 问题: internal/security/provider.go 声明了 AuditRepo、KillSwitchMetrics、GuardProvider 三个消费端/生产端接口，但在仓库实际实现中，audit_trail.go 直接消费了 protocol.AuditRepository，killswitch.go 消费了回调函数，导致这三个接口成为无任何生产及测试调用的死代码，违反了 docs/specs/00-Constitution.md R1.4 关于接口不许无真实消费方的要求及维度 G-bis 接线可达性原则。
- 证据: internal/security/provider.go:27-52
  ```go
  type AuditRepo interface {
  	Insert(ctx context.Context, record *AuditRecord) error
  	LoadSince(ctx context.Context, afterTimestampMicro int64) ([]*AuditRecord, error)
  }
  ```
- 修复方向提示: 清理 internal/security/provider.go 中无调用的冗余接口或将真实消费点收口迁移至这些接口。

---

## 已审文件清单

- internal/security/audit_integrity.go
- internal/security/audit_trail.go
- internal/security/classifier/classifier.go
- internal/security/classifier/rules.go
- internal/security/credential/vault.go
- internal/security/guard/anomaly_distance_filter.go
- internal/security/guard/factuality_guard.go
- internal/security/guard/factuality_guard_numeric.go
- internal/security/guard/pii_desensitizer.go
- internal/security/guard/pii_detector.go
- internal/security/guard/pii_token_vault.go
- internal/security/guard/random.go
- internal/security/guard/sic.go
- internal/security/guard/system_prompt_guard.go
- internal/security/killswitch.go
- internal/security/killswitch_recovery.go
- internal/security/killswitch_seal_info.go
- internal/security/network/local_only.go
- internal/security/network/local_only_allowlist_loader.go
- internal/security/network/local_only_darwin.go
- internal/security/network/local_only_linux.go
- internal/security/network/local_only_other.go
- internal/security/network/local_only_windows.go
- internal/security/network/safe_dialer.go
- internal/security/network/safe_dialer_capability.go
- internal/security/policy/cedar_ffi.go
- internal/security/policy/gate.go
- internal/security/policy/gate_builtin_rules.go
- internal/security/policy/gate_cedar.go
- internal/security/policy/gate_egress.go
- internal/security/provider.go
- internal/security/taint/taint.go
- internal/security/taint/taint_gate.go
- internal/security/taint/taint_sanitizer.go
- internal/security/token/capability_token.go
- internal/security/token/exemption_token.go
- internal/security/token/exemption_vault.go
- internal/llm/adapter/anthropic.go
- internal/llm/adapter/anthropic_request.go
- internal/llm/adapter/client.go
- internal/llm/adapter/control_vector_store.go
- internal/llm/adapter/deepseek.go
- internal/llm/adapter/embedding.go
- internal/llm/adapter/google.go
- internal/llm/adapter/google_request.go
- internal/llm/adapter/http_client.go
- internal/llm/adapter/local.go
- internal/llm/adapter/ollama.go
- internal/llm/adapter/openai.go
- internal/llm/adapter/steering.go
- internal/llm/adapter/stream.go
- internal/llm/adapter/training.go
- internal/llm/adapter/training_sample_collector.go
- internal/llm/circuit_breaker.go
- internal/llm/credential_pool.go
- internal/llm/dynamic_embedder.go
- internal/llm/error_classifier.go
- internal/llm/media_opt.go
- internal/llm/modelregistry/registry.go
- internal/llm/ollamamgr/manager.go
- internal/llm/provider_registry.go
- internal/llm/rate_tracker.go
- internal/llm/router.go
- internal/llm/router_failover.go
- internal/llm/router_stream.go
- internal/llm/safecall/safecall.go
- internal/llm/stream_guard.go
- internal/llm/stt/dlopen_unix.go
- internal/llm/stt/dlopen_windows.go
- internal/llm/stt/downloader.go
- internal/llm/stt/sherpa.go
- internal/llm/tokenizer.go
- internal/llm/tts/downloader.go
- internal/llm/tts/edge.go
- internal/llm/tts/http.go
- internal/llm/tts/provider.go
- internal/llm/tts/sherpa.go
- internal/llm/tts/wav.go
- internal/llm/window_breaker.go

## 明确未覆盖的范围

无

## 审了但无发现的模块

- internal/security/guard (已全覆盖通读，未发现新增问题)
- internal/security/classifier (规则定义清晰，未发现问题)
- internal/security/credential (Vault 明文清理与并发锁正常)
- internal/llm/stt (Sherpa FFI 与资源下载逻辑正常)
- internal/llm/tts (Edge/Sherpa 音频流正常)
