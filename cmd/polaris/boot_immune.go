package main

import (
	"context"

	"github.com/polarisagi/polaris/internal/immune"
)

type dummyImmuneGateway struct{}

func (g *dummyImmuneGateway) Scan(ctx context.Context, agentID string, scanType string) (any, error) {
	// Empty implementation per requirements
	return nil, nil
}

func NewImmuneGateway() immune.ImmuneGateway {
	return &dummyImmuneGateway{}
}
