package schema

import "embed"

// FS 是一个嵌入的文件系统，包含了数据库初始化的所有的 .sql schema 迁移文件。
// M2 Storage 的 SQLiteStore 使用该文件系统进行自动化的表结构建立。
//
//go:embed *.sql
var FS embed.FS
