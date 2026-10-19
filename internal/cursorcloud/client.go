package cursorcloud

import "context"

// EnvAPIKey is the only env var this package reads for credentials.
const EnvAPIKey = "CURSOR_API_KEY"

type CreateAgentRequest struct {
	Repo   string // repository URL
	Ref    string // commit SHA to review
	Prompt string // full review prompt
	Model  string // empty = account default
	Title  string
}

type AgentStatus string

const (
	StatusRunning  AgentStatus = "RUNNING"
	StatusFinished AgentStatus = "FINISHED"
	StatusError    AgentStatus = "ERROR"
	StatusExpired  AgentStatus = "EXPIRED"
)

type Agent struct {
	ID     string
	Status AgentStatus
	Title  string
}

// Client is the minimal Cursor cloud agents API surface roborev needs.
type Client interface {
	CreateAgent(ctx context.Context, req CreateAgentRequest) (Agent, error)
	GetAgent(ctx context.Context, id string) (Agent, error)
	// FinalMessage returns the agent's last assistant message (the review text).
	FinalMessage(ctx context.Context, id string) (string, error)
	ArchiveAgent(ctx context.Context, id string) error
	ListAgents(ctx context.Context) ([]Agent, error)
}
