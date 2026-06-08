// Package substrate 是 L0 基础设施层。
// 涵盖模块:
//   - M1 Inference Runtime (LLM 厂商路由、Provider 适配、结构化输出)
//   - M2 Storage Fabric (多引擎存储编织层、EventLog 真相源、MutationBus)
//   - M3 Observability (OTel 全链路追踪、TokenBurnRate/SurpriseIndex 指标)
//   - M11 Policy & Safety (Taint Tracking 污点追踪、Cedar 策略引擎、KillSwitch)
//
// 不变量: [HE-Rule-1] 可观测优先, [HE-Rule-2] 可验证执行, [HE-Rule-6] State-in-DB。
// 本层不依赖 cognition/action/swarm/governance/edge 中的任何包。
package substrate
