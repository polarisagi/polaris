#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

make build
# 架构一致性用例以私有设计文档为输入；公开快照不携带该文档，故只跳过这类文档对照用例。
go test -skip '^(TestFSM_Spec|TestSpec.*)$' ./internal/... ./cmd/... ./pkg/...
cargo test --manifest-path rust/substrate/Cargo.toml
