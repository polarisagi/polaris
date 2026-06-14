// Package governance 是 L3 治理层。
// 涵盖模块与子包职责:
//   - eval/: EvalCase 存取 + Runner 执行（BehaviorType / FalsifiabilityScore 体系）
//   - policy/: Cedar Policy Engine 封装
//   - synthetic/: 合成评测用例生成
//
// 不变量: [HE-Rule-4] Eval 第 0 行存在，失败 = PR 阻塞。
// 依赖: 全部 L0 + L1 + L2 模块。
package governance
