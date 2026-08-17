# L-03（OriginTaintLevel 禁写死常量）存量基线 —— 当前存量 0。
#
# 2026-08-17：本文件原有 5 条。其中 memory_context.go:361 与 rag_retrieval.go:76/311
# 早已在代码里修好，条目却留着，是只挂不摘的死抑制；memory_context.go:187/330
# 两条真违规已改为 types.PropagateTaint(types.TaintMedium, sCtx.GlobalTaintLevel)。
# 文件保留仅为记录零存量与本条历史，新增条目等于给污点降级开后门，请先改代码。
