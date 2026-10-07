package daemon

import (
	"context"
	"encoding/json/v2"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ghpkg "go.kenn.io/roborev/internal/github"
)

func TestPersonalMissingRequiredMemberCannotApprove(t *testing.T) {
	for _, count := range []int{0, 2} {
		t.Run("expected_"+strconv.Itoa(count), func(t *testing.T) {
			h := personalHarness(t)
			h.Cfg.CI.Personal.Mode = "publish"
			panel := seedPersonalPanel(t, h)
			synth, err := h.DB.GetSynthesisJob(panel.PanelRunUUID)
			require.NoError(t, err)
			require.NotNil(t, synth)
			// One successful member remains. Two means a required row disappeared;
			// zero means the frozen completeness metadata is missing or legacy.
			snapshot, err := json.Marshal(ciACPExecutionConfig{Personal: personalPolicySnapshot(h.Cfg), PersonalMemberCount: count})
			require.NoError(t, err)
			_, err = h.DB.Exec(`UPDATE review_jobs SET panel_member_config_json=? WHERE id=?`, string(snapshot), synth.ID)
			require.NoError(t, err)
			event := ""
			h.Poller.submitPersonalReviewFn = func(_ context.Context, _ string, _ int, submission ghpkg.ReviewSubmission) (int64, error) {
				event = submission.Event
				return 42, nil
			}
			h.Poller.postPanelRun(t.Context(), panel)
			assert.Equal(t, "COMMENT", event)
		})
	}
}
