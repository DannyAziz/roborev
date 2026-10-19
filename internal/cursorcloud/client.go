package cursorcloud

import (
	"context"
	"errors"
)

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
	// StatusCreating is the v0 launch status. It is not terminal.
	StatusCreating AgentStatus = "CREATING"
	StatusRunning  AgentStatus = "RUNNING"
	StatusFinished AgentStatus = "FINISHED"
	StatusError    AgentStatus = "ERROR"
	StatusExpired  AgentStatus = "EXPIRED"
)

// ErrNoAssistantMessage is returned when the conversation has no
// assistant_message. Callers must fail the review; there is no empty fallback.
var ErrNoAssistantMessage = errors.New("cursor cloud final message: no assistant message")

type Agent struct {
	ID               string
	Status           AgentStatus
	Title            string
	SourceRepository string
	SourceRef        string
}

// Client is the minimal Cursor cloud agents API surface roborev needs.
type Client interface {
	CreateAgent(ctx context.Context, req CreateAgentRequest) (Agent, error)
	GetAgent(ctx context.Context, id string) (Agent, error)
	// FinalMessage returns the agent's last assistant message (the review text).
	// It returns an error when the conversation has no assistant_message.
	FinalMessage(ctx context.Context, id string) (string, error)
	// UserPrompt returns the user_message texts for id. List Agents does not
	// include the prompt, so reconciliation reads the conversation.
	// https://cursor.com/docs/cloud-agent/api/v0#agent-conversation
	UserPrompt(ctx context.Context, id string) (string, error)
	// StopAgent pauses a running agent (POST /v0/agents/{id}/stop). The v0
	// docs say this does not delete the agent and do not say it ends billing.
	// https://cursor.com/docs/cloud-agent/api/v0#stop-an-agent
	StopAgent(ctx context.Context, id string) error
	// DeleteAgent permanently deletes the agent (DELETE /v0/agents/{id}).
	// The v0 docs call this permanent and do not say it stops billing.
	// https://cursor.com/docs/cloud-agent/api/v0#delete-an-agent
	DeleteAgent(ctx context.Context, id string) error
	ListAgents(ctx context.Context) ([]Agent, error)
}
