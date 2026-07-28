package chat

import (
	apptypes "github.com/polarisagi/polaris/pkg/types"
)

// ContextBuilder 承担原来由 ChatHandler 越界处理的上下文构建职责（GD-13-001）
type ContextBuilder struct{}

// NewContextBuilder 创建上下文构建器
func NewContextBuilder() *ContextBuilder {
	return &ContextBuilder{}
}

// Build 占位实现
func (b *ContextBuilder) Build() []apptypes.Message {
	return nil
}
