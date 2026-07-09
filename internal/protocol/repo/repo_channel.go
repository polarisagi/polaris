package repo

import "context"

type ChannelRow struct {
	ID            string
	Name          string
	Type          string
	Enabled       bool
	ConfigJSON    string
	WebhookSecret string
	CreatedAt     string
	UpdatedAt     string
}

type ChannelRepository interface {
	CreateChannel(ctx context.Context, row ChannelRow) error
	UpdateChannel(ctx context.Context, row ChannelRow) (bool, error)
	DeleteChannel(ctx context.Context, id string) error
	ListChannels(ctx context.Context) ([]ChannelRow, error)
	GetChannel(ctx context.Context, id string) (*ChannelRow, error)
}
