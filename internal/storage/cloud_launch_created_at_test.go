package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCloudLaunchInsertStoresCreatedAt(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, _, job := createJobChain(t, db, "/tmp/launch-created", "abc123")
	_, err := db.Exec(`UPDATE review_jobs SET status = 'running', worker_id = 'w' WHERE id = ?`, job.ID)
	require.NoError(t, err)
	require.NoError(t, db.BeginCloudLaunch(job.ID, "w", "created-tok", "https://example.com/acme/repo.git"))

	var created string
	require.NoError(t, db.QueryRow(`SELECT created_at FROM review_job_cloud_launches WHERE launch_token = 'created-tok'`).Scan(&created))
	assert.NotEmpty(t, created)
}
