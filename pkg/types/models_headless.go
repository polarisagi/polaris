package types

type Intent struct {
	Query      string
	WorkingDir string
	Env        map[string]string
}

type AgentResult struct {
	Output    string
	LatencyMs int64
}

type HeadlessOption func(*HeadlessOptions)

type HeadlessOptions struct {
	MaxRounds int
}
