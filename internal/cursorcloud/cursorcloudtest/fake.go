// Package cursorcloudtest is an in-memory cursorcloud.Client for tests.
package cursorcloudtest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"go.kenn.io/roborev/internal/cursorcloud"
)

const (
	MethodCreate = "CreateAgent"
	MethodGet    = "GetAgent"
	MethodFinal  = "FinalMessage"
	MethodStop   = "StopAgent"
	MethodDelete = "DeleteAgent"
	MethodPrompt = "UserPrompt"
	MethodList   = "ListAgents"
)

// Call is one invocation recorded by Fake.
type Call struct {
	Method  string
	ID      string
	Request cursorcloud.CreateAgentRequest
}

// Fake implements cursorcloud.Client in memory.
//
// CreateStatus is the status stored for agents created after it is set; empty
// means CREATING, matching a v0 launch. Final is the assistant text used when an id has no per-id
// message. Error is returned by every method that has no MethodErrors entry.
// MethodErrors is keyed by the Method* constants. Set fields before calling;
// the methods themselves are safe for concurrent use.
type Fake struct {
	mu sync.Mutex

	CreateStatus cursorcloud.AgentStatus
	Final        string
	Error        error
	MethodErrors map[string]error
	// DeleteFailures makes the next N DeleteAgent calls fail, then succeed.
	DeleteFailures int

	agents  map[string]cursorcloud.Agent
	order   []string
	finals  map[string]string
	prompts map[string]string
	scripts map[string][]cursorcloud.AgentStatus
	seq     int
	calls   []Call
}

// Seed inserts or replaces an agent without recording a call.
func (f *Fake) Seed(agent cursorcloud.Agent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if _, ok := f.agents[agent.ID]; !ok {
		f.order = append(f.order, agent.ID)
	}
	f.agents[agent.ID] = agent
}

// SetStatus stores status for id, creating a shell agent when id is new.
func (f *Fake) SetStatus(id string, status cursorcloud.AgentStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	agent, ok := f.agents[id]
	if !ok {
		f.order = append(f.order, id)
		agent.ID = id
	}
	agent.Status = status
	f.agents[id] = agent
}

// ScriptStatuses makes successive GetAgent calls return statuses in order.
// The stored status updates to each returned value. After the script is
// consumed, GetAgent returns the last stored status.
func (f *Fake) ScriptStatuses(id string, statuses ...cursorcloud.AgentStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	f.scripts[id] = append([]cursorcloud.AgentStatus(nil), statuses...)
	if _, ok := f.agents[id]; !ok {
		f.agents[id] = cursorcloud.Agent{ID: id, Status: cursorcloud.StatusCreating}
		f.order = append(f.order, id)
	}
}

// SetFinalMessage sets the assistant text FinalMessage returns for id.
func (f *Fake) SetFinalMessage(id, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	f.finals[id] = text
}

// Calls returns a copy of recorded invocations, including ones that returned
// an injected error.
func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Call, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *Fake) CreateAgent(ctx context.Context, req cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	if err := rejectCtx(ctx); err != nil {
		return cursorcloud.Agent{}, err
	}
	if strings.TrimSpace(req.Repo) == "" {
		return cursorcloud.Agent{}, errors.New("cursor cloud create agent: repository is empty")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return cursorcloud.Agent{}, errors.New("cursor cloud create agent: prompt is empty")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if err := f.fail(ctx, MethodCreate, "", req); err != nil {
		return cursorcloud.Agent{}, err
	}
	f.seq++
	status := f.CreateStatus
	if status == "" {
		status = cursorcloud.StatusCreating
	}
	// Title is not echoed. v0 does not accept it and returns a generated name.
	agent := cursorcloud.Agent{
		ID:               fmt.Sprintf("bc_fake_%d", f.seq),
		Status:           status,
		SourceRepository: req.Repo,
		SourceRef:        req.Ref,
	}
	f.agents[agent.ID] = agent
	f.prompts[agent.ID] = req.Prompt
	f.order = append(f.order, agent.ID)
	return agent, nil
}

func (f *Fake) GetAgent(ctx context.Context, id string) (cursorcloud.Agent, error) {
	if err := rejectCtx(ctx); err != nil {
		return cursorcloud.Agent{}, err
	}
	if err := invalidAgentID(id); err != nil {
		return cursorcloud.Agent{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if err := f.fail(ctx, MethodGet, id, cursorcloud.CreateAgentRequest{}); err != nil {
		return cursorcloud.Agent{}, err
	}
	agent, ok := f.agents[id]
	if !ok {
		return cursorcloud.Agent{}, notFound("get agent")
	}
	if script := f.scripts[id]; len(script) > 0 {
		agent.Status = script[0]
		f.scripts[id] = script[1:]
		f.agents[id] = agent
	}
	return agent, nil
}

func (f *Fake) FinalMessage(ctx context.Context, id string) (string, error) {
	if err := rejectCtx(ctx); err != nil {
		return "", err
	}
	if err := invalidAgentID(id); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if err := f.fail(ctx, MethodFinal, id, cursorcloud.CreateAgentRequest{}); err != nil {
		return "", err
	}
	if _, ok := f.agents[id]; !ok {
		return "", notFound("final message")
	}
	if text, ok := f.finals[id]; ok {
		return text, nil
	}
	if f.Final != "" {
		return f.Final, nil
	}
	return "", cursorcloud.ErrNoAssistantMessage
}

func (f *Fake) UserPrompt(ctx context.Context, id string) (string, error) {
	if err := rejectCtx(ctx); err != nil {
		return "", err
	}
	if err := invalidAgentID(id); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if err := f.fail(ctx, MethodPrompt, id, cursorcloud.CreateAgentRequest{}); err != nil {
		return "", err
	}
	if _, ok := f.agents[id]; !ok {
		return "", notFound("conversation")
	}
	return f.prompts[id], nil
}

func (f *Fake) StopAgent(ctx context.Context, id string) error {
	if err := rejectCtx(ctx); err != nil {
		return err
	}
	if err := invalidAgentID(id); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if err := f.fail(ctx, MethodStop, id, cursorcloud.CreateAgentRequest{}); err != nil {
		return err
	}
	if _, ok := f.agents[id]; !ok {
		return notFound("stop agent")
	}
	return nil
}

func (f *Fake) DeleteAgent(ctx context.Context, id string) error {
	if err := rejectCtx(ctx); err != nil {
		return err
	}
	if err := invalidAgentID(id); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if f.DeleteFailures > 0 {
		f.DeleteFailures--
		f.calls = append(f.calls, Call{Method: MethodDelete, ID: id})
		return errors.New("delete failed")
	}
	if err := f.fail(ctx, MethodDelete, id, cursorcloud.CreateAgentRequest{}); err != nil {
		return err
	}
	if _, ok := f.agents[id]; !ok {
		// A second DELETE of an agent this fake already removed is the v0
		// 404. deleteCursorCloudAgent treats that as already gone.
		return notFound("delete agent")
	}
	delete(f.agents, id)
	delete(f.finals, id)
	delete(f.prompts, id)
	delete(f.scripts, id)
	next := make([]string, 0, len(f.order))
	for _, existing := range f.order {
		if existing != id {
			next = append(next, existing)
		}
	}
	f.order = next
	return nil
}

func (f *Fake) ListAgents(ctx context.Context) ([]cursorcloud.Agent, error) {
	if err := rejectCtx(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.init()
	if err := f.fail(ctx, MethodList, "", cursorcloud.CreateAgentRequest{}); err != nil {
		return nil, err
	}
	out := make([]cursorcloud.Agent, 0, len(f.order))
	for _, id := range f.order {
		if agent, ok := f.agents[id]; ok {
			out = append(out, agent)
		}
	}
	return out, nil
}

func (f *Fake) init() {
	if f.agents == nil {
		f.agents = map[string]cursorcloud.Agent{}
	}
	if f.finals == nil {
		f.finals = map[string]string{}
	}
	if f.prompts == nil {
		f.prompts = map[string]string{}
	}
	if f.scripts == nil {
		f.scripts = map[string][]cursorcloud.AgentStatus{}
	}
}

func rejectCtx(ctx context.Context) error {
	if ctx == nil {
		return errors.New("cursor cloud: nil context")
	}
	return nil
}

func invalidAgentID(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("cursor cloud: agent id is empty")
	}
	if strings.ContainsAny(id, "/\\?#") || id == "." || id == ".." {
		return errors.New("cursor cloud: agent id is invalid")
	}
	return nil
}

func notFound(op string) error {
	return &cursorcloud.APIError{Op: op, StatusCode: http.StatusNotFound, Message: "not found"}
}

func (f *Fake) fail(ctx context.Context, method, id string, req cursorcloud.CreateAgentRequest) error {
	f.calls = append(f.calls, Call{Method: method, ID: id, Request: req})
	if f.MethodErrors != nil {
		if err := f.MethodErrors[method]; err != nil {
			return err
		}
	}
	if f.Error != nil {
		return f.Error
	}
	if ctx != nil {
		return ctx.Err()
	}
	return nil
}

var _ cursorcloud.Client = (*Fake)(nil)
