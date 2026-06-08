// Package governance 是 L3 治理层。
// 涵盖模块:
//   - M12 Eval Harness (五层 Eval Pyramid、轨迹录制/回放、影子执行、回归检测)
//
// 不变量: [HE-Rule-4] Eval 第 0 行存在，失败 = PR 阻塞。
// 依赖: 全部 L0 + L1 + L2 模块。
package governance
