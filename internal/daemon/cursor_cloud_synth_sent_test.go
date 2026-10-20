package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/cursorcloud/cursorcloudtest"
)

// A finished cloud synthesis rejected after its create returned must not
// launch again, whatever rejects it: validateLiveDocument, a no-verdict
// output, or a decode error. The backup is cursor-cloud too, so a rejection
// that is not marked sent fails over or retries into a second CreateAgent.
func TestProcessSynthesisCursorCloudRejectedResultDoesNotRelaunch(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret")
	const repo = "https://example.com/acme/repo.git"
	for _, tc := range []struct {
		name, final, backup string
	}{
		{name: "live-document", final: `{"schema_version":2,"summary":"blocked","verdict":"unable_to_review","findings":[]}`, backup: "test"},
		{name: "live-document-cloud-backup", final: `{"schema_version":2,"summary":"blocked","verdict":"unable_to_review","findings":[]}`, backup: agent.CursorCloudHost},
		{name: "no-verdict-cloud-backup", final: "I am unable to read the diff file because it is ignored.", backup: agent.CursorCloudHost},
		{name: "decode-cloud-backup", final: `{"not_a_synthesis":true}`, backup: agent.CursorCloudHost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &cursorcloudtest.Fake{CreateStatus: cursorcloud.StatusFinished, Final: tc.final}
			prev, err := agent.Get(agent.CursorCloudHost)
			require.NoError(t, err)
			agent.Register(&agent.CursorCloudAgent{Client: fake, RepoURL: repo})
			t.Cleanup(func() { agent.Register(prev) })

			wtc := newWorkerTestContext(t, 1)
			wtc.Pool.retryBackoff = 0
			wtc.GitRepo.Run("remote", "add", "origin", repo)
			synth := claimSynthesis(t, wtc, "synth-rejected-"+tc.name, agent.CursorCloudHost, tc.backup)
			originalID := synth.ID

			for range maxRetries + 2 {
				if synth.Agent != agent.CursorCloudHost {
					break
				}
				wtc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)
				next, claimErr := wtc.DB.ClaimJob(testWorkerID)
				require.NoError(t, claimErr)
				if next == nil {
					break
				}
				synth = next
			}
			assert.Equal(t, 1, createCountFromFake(fake))
			stored, err := wtc.DB.GetJobByID(originalID)
			require.NoError(t, err)
			assert.Equal(t, 0, stored.RetryCount)
		})
	}
}

// A cloud synthesis whose document is valid but cannot be stored must not
// launch again. completeSynthesisLocked routes the storage error through the
// ordinary retry path, and a retry of a sent attempt POSTs a second agent.
func TestProcessSynthesisCursorCloudStoreFailureDoesNotRelaunch(t *testing.T) {
	t.Setenv(cursorcloud.EnvAPIKey, "cursor-test-secret")
	wtc, fake := cursorCloudRestartFixture(t)
	wtc.Pool.retryBackoff = 0
	fake.CreateStatus = cursorcloud.StatusFinished
	fake.Final = backupCloudSynthesisFinal
	synth := claimSynthesis(t, wtc, "synth-store-failure", agent.CursorCloudHost, "test")
	originalID := synth.ID
	_, err := wtc.DB.Exec(`CREATE TRIGGER fail_review_insert BEFORE INSERT ON reviews
		BEGIN SELECT RAISE(ABORT, 'synthetic store failure'); END`)
	require.NoError(t, err)

	for range maxRetries + 2 {
		if synth.Agent != agent.CursorCloudHost {
			break
		}
		wtc.Pool.processSynthesisJob(context.Background(), testWorkerID, synth)
		next, claimErr := wtc.DB.ClaimJob(testWorkerID)
		require.NoError(t, claimErr)
		if next == nil {
			break
		}
		synth = next
	}
	assert.Equal(t, 1, createCountFromFake(fake))
	stored, err := wtc.DB.GetJobByID(originalID)
	require.NoError(t, err)
	assert.Equal(t, 0, stored.RetryCount)
}
