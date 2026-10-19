package cursorcloudtest

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/cursorcloud"
)

func TestFakeLifecycleAndCalls(t *testing.T) {
	fake := &Fake{CreateStatus: cursorcloud.StatusRunning}
	ctx := context.Background()
	req := cursorcloud.CreateAgentRequest{
		Repo:   "https://github.com/acme/widgets",
		Ref:    "abc",
		Prompt: "review",
		Title:  "widgets review",
	}
	created, err := fake.CreateAgent(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, "bc_fake_1", created.ID)
	assert.Equal(t, cursorcloud.StatusRunning, created.Status)
	assert.Equal(t, "widgets review", created.Title)

	fake.ScriptStatuses(created.ID, cursorcloud.StatusRunning, cursorcloud.StatusFinished)
	first, err := fake.GetAgent(ctx, created.ID)
	require.NoError(t, err)
	second, err := fake.GetAgent(ctx, created.ID)
	require.NoError(t, err)
	third, err := fake.GetAgent(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, cursorcloud.StatusRunning, first.Status)
	assert.Equal(t, cursorcloud.StatusFinished, second.Status)
	assert.Equal(t, cursorcloud.StatusFinished, third.Status)

	fake.SetFinalMessage(created.ID, "ship it")
	text, err := fake.FinalMessage(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "ship it", text)

	other, err := fake.CreateAgent(ctx, req)
	require.NoError(t, err)
	listed, err := fake.ListAgents(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	assert.Equal(t, created.ID, listed[0].ID)
	assert.Equal(t, other.ID, listed[1].ID)

	calls := fake.Calls()
	require.GreaterOrEqual(t, len(calls), 2)
	assert.Equal(t, MethodCreate, calls[0].Method)
	assert.Equal(t, req, calls[0].Request)
	assert.Equal(t, MethodGet, calls[1].Method)
	assert.Equal(t, created.ID, calls[1].ID)
}

func TestFakeDefaultFinalAndErrors(t *testing.T) {
	fake := &Fake{Final: "default review"}
	fake.Seed(cursorcloud.Agent{ID: "bc_seed", Status: cursorcloud.StatusError, Title: "seeded"})
	ctx := context.Background()

	text, err := fake.FinalMessage(ctx, "bc_seed")
	require.NoError(t, err)
	assert.Equal(t, "default review", text)

	fake.SetStatus("bc_seed", cursorcloud.StatusExpired)
	got, err := fake.GetAgent(ctx, "bc_seed")
	require.NoError(t, err)
	assert.Equal(t, cursorcloud.StatusExpired, got.Status)

	boom := errors.New("injected")
	fake.MethodErrors = map[string]error{MethodGet: boom}
	_, err = fake.GetAgent(ctx, "bc_seed")
	require.ErrorIs(t, err, boom)
	listed, err := fake.ListAgents(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)

	fake.Error = errors.New("all down")
	_, err = fake.ListAgents(ctx)
	require.ErrorContains(t, err, "all down")
	calls := fake.Calls()
	assert.Equal(t, MethodList, calls[len(calls)-1].Method)

	_, err = fake.FinalMessage(ctx, "missing")
	require.Error(t, err)
	fake.Error = nil
	fake.MethodErrors = nil
	_, err = fake.FinalMessage(ctx, "missing")
	require.ErrorContains(t, err, "no final message")
}

func TestFakeDefaultsToCreating(t *testing.T) {
	fake := &Fake{}
	created, err := fake.CreateAgent(context.Background(), cursorcloud.CreateAgentRequest{
		Repo: "https://example.com/acme/repo.git", Prompt: "review",
	})
	require.NoError(t, err)
	assert.Equal(t, cursorcloud.StatusCreating, created.Status)
}
