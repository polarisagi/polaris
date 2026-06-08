package server

// sse_media.go — 网关层多模态内容前置门控
//
// 图片压缩/降采样逻辑已移至 L0 推理层：
//   pkg/substrate/inference/media_opt.go → normalizeInferRequest()
//
// 原因：压缩是 Provider-agnostic 的通用需求，放在 InferenceRouter 可自动覆盖
// 所有调用方（Gateway / Cognition Kernel / MCP Extensions / Swarm），
// 无需各调用方单独实现。
//
// 本文件仅保留：视频大小门控常量（网关层业务决策，不下沉到推理层）。

const (
	// maxVideoInlineBytes Gemini inlineData 视频大小上限（20MB）。
	// 超过此值应走 Gemini File API 上传后使用 URI，当前不支持，拒绝处理。
	maxVideoInlineBytes = 20 * 1024 * 1024
)
