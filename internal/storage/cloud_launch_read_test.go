package storage

import (
	"encoding/json/jsontext"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetReviewByJobIDKeepsLaunchOrphanNote(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, _, job := createJobChain(t, db, t.TempDir(), "read-sha")
	_, err := db.Exec(`UPDATE review_jobs SET status = 'running' WHERE id = ?`, job.ID)
	require.NoError(t, err)
	require.NoError(t, db.CompleteJobResult(job.ID, "test", "prompt", ReviewCompletion{
		StructuredOutput: jsontext.Value(`{"schema_version":2,"summary":"kept","verdict":"pass","findings":[]}`),
	}))
	_, err = db.Exec(`INSERT INTO review_job_cloud_launches
		(job_id, launch_token, orphan_note) VALUES (?, 'read-tok', ?)`,
		job.ID, CloudLaunchPendingNote)
	require.NoError(t, err)

	review, err := db.GetReviewByJobID(job.ID)
	require.NoError(t, err)
	require.NotNil(t, review.Job)
	assert.Contains(t, review.Job.OrphanWarning, "orphan risk",
		"GetReviewByJobID dropped the orphan warning")
	assert.Contains(t, review.Job.Error, "orphan risk",
		"GetReviewByJobID dropped the orphan warning")
}
