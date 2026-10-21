package agent

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/cursorcloud/cursorcloudtest"
)

// TestCursorCloudInFlightCreateWithoutVMIsNotCleaned is the post-send create
// that never produced a VM. Reconcile lists and does not delete, so the
// launch is not cleaned. A 60s window would hide a revert that marks cleaned
// on the error alone; the shortened window still takes the first list.
func TestCursorCloudInFlightCreateWithoutVMIsNotCleaned(t *testing.T) {
	prev := cursorCloudReconcileWindow
	cursorCloudReconcileWindow = time.Millisecond
	t.Cleanup(func() { cursorCloudReconcileWindow = prev })

	var cleaned int
	client := &deadlineCreate{Fake: &cursorcloudtest.Fake{}}
	cloud := &CursorCloudAgent{
		Client:  client,
		RepoURL: "https://example.com/acme/repo.git",
		MarkLaunchCleaned: func(string) error {
			cleaned++
			return nil
		},
	}
	cloud.SetLaunchLimitsForTest(2, 5)
	_, err := cloud.Review(
		context.Background(), t.TempDir(),
		"0123456789abcdef0123456789abcdef01234567", "prompt", nil,
	)
	require.Error(t, err)
	assert.Equal(t, 0, cleaned)
	assert.Empty(t, cursorCloudDeleteIDs(client.Fake))
}

// TestCursorCloudReconcileDeleteCleans is a create whose response was lost
// but whose VM was listed. Reconcile deletes it and cleans the launch.
func TestCursorCloudReconcileDeleteCleans(t *testing.T) {
	var cleaned int
	client := &lostCreate{Fake: &cursorcloudtest.Fake{}}
	cloud := &CursorCloudAgent{
		Client:  client,
		RepoURL: "https://example.com/acme/repo.git",
		MarkLaunchCleaned: func(string) error {
			cleaned++
			return nil
		},
	}
	_, err := cloud.Review(
		context.Background(), t.TempDir(),
		"0123456789abcdef0123456789abcdef01234567", "prompt", nil,
	)
	require.Error(t, err)
	assert.Equal(t, 1, cleaned)
	assert.Len(t, cursorCloudDeleteIDs(client.Fake), 1)
}

// lostCreate creates the VM and then loses the response.
type lostCreate struct {
	*cursorcloudtest.Fake
}

func (c *lostCreate) CreateAgent(ctx context.Context, req cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	if _, err := c.Fake.CreateAgent(ctx, req); err != nil {
		return cursorcloud.Agent{}, err
	}
	return cursorcloud.Agent{}, context.DeadlineExceeded
}

type deadlineCreate struct {
	*cursorcloudtest.Fake
}

func (c *deadlineCreate) CreateAgent(context.Context, cursorcloud.CreateAgentRequest) (cursorcloud.Agent, error) {
	return cursorcloud.Agent{}, context.DeadlineExceeded
}

func cursorCloudDeleteIDs(fake *cursorcloudtest.Fake) []string {
	var ids []string
	for _, call := range fake.Calls() {
		if call.Method == cursorcloudtest.MethodDelete {
			ids = append(ids, call.ID)
		}
	}
	return ids
}
