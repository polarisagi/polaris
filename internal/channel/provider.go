package channel

// 本文件用于声明 channel 包对外部模块的消费端接口（Consumer-side Interfaces）。
//
// 当前 channel 包没有需要在此声明的外部依赖：
//   - 原 AuthChecker/ChannelAuthChecker 已于 2026-07-12 按死代码移除；
//   - 原 ChatRepo / AgentInfer 同样全仓零实现、零消费（消息落库与 Agent 推理
//     由 gateway 层 sysadmin/channelsadmin 经 session.Orchestrator 完成），
//     GR-10.2-004 按死代码移除，不臆造接线。
//
// 新增外部依赖时在此声明窄接口，由组合根注入。
