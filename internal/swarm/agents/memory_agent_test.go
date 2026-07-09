package agents

import (
	_ "github.com/mattn/go-sqlite3"
)

// mockSurreal 为测试提供 SurrealWriterInterface 空实现。
// 原定义在 extension_librarian_handler_test.go（已迁移到 knowledge/connector 包），
// 此处保留副本供 MemoryAgent 测试使用。
