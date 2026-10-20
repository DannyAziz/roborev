package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMarkCloudLaunchCleanedLeavesOtherRows is R9b. The clean UPDATE is
// scoped to one launch. Dropping that scope, or the token predicate that
// chooses which ids to clean, marks a different token on the same job and a
// different job cleaned. Both of those rows stay cleaned=0.
func TestMarkCloudLaunchCleanedLeavesOtherRows(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	t.Cleanup(func() { db.Close() })
	_, _, jobA := createJobChain(t, db, t.TempDir(), "abc123")
	_, _, jobB := createJobChain(t, db, t.TempDir(), "def456")

	insertLaunch := func(jobID int64, token, note string) int64 {
		t.Helper()
		res, err := db.Exec(`INSERT INTO review_job_cloud_launches
			(job_id, launch_token, orphan_note) VALUES (?, ?, ?)`, jobID, token, note)
		require.NoError(t, err)
		id, err := res.LastInsertId()
		require.NoError(t, err)
		return id
	}
	idA := insertLaunch(jobA.ID, "tok-a", "note-a")
	idB := insertLaunch(jobA.ID, "tok-b", "note-b")
	idC := insertLaunch(jobB.ID, "tok-c", "note-c")

	require.NoError(t, db.MarkCloudLaunchCleanedByToken("tok-a"))

	type row struct {
		id    int64
		token string
		note  string
		want  int
	}
	assert := assert.New(t)
	for _, want := range []row{
		{id: idA, token: "tok-a", note: "", want: 1},
		{id: idB, token: "tok-b", note: "note-b", want: 0},
		{id: idC, token: "tok-c", note: "note-c", want: 0},
	} {
		var cleaned int
		var note string
		require.NoError(t, db.QueryRow(
			`SELECT cleaned, orphan_note FROM review_job_cloud_launches WHERE id = ?`, want.id,
		).Scan(&cleaned, &note))
		assert.Equal(want.want, cleaned, want.token)
		assert.Equal(want.note, note, want.token)
	}
}
