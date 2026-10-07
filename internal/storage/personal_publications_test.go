package storage

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersonalPublicationIntentIsImmutable(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	run := testUUID("personal-publication")
	first, err := db.ReservePersonalPublication(run, "original", false)
	require.NoError(t, err)
	require.True(t, first)
	second, err := db.ReservePersonalPublication(run, "replacement", false)
	require.NoError(t, err)
	require.False(t, second)
	require.NoError(t, db.MarkPersonalPublicationUncertain(run))
	p, err := db.GetPersonalPublication(run)
	require.NoError(t, err)
	require.NotNil(t, p)
	assert := assert.New(t)
	assert.Equal("original", p.SubmissionJSON)
	assert.Equal("uncertain", p.Phase)
	require.NoError(t, db.MarkPersonalPublicationSubmitted(run, 42))
	require.NoError(t, db.MarkPersonalPublicationUncertain(run))
	p, err = db.GetPersonalPublication(run)
	require.NoError(t, err)
	assert.Equal("submitted", p.Phase)
	assert.Equal(int64(42), p.ReviewID)
}

func TestPersonalPreviewCannotBecomeSubmission(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	run := testUUID("personal-preview")
	reserved, err := db.ReservePersonalPublication(run, "preview", true)
	require.NoError(t, err)
	require.True(t, reserved)
	require.NoError(t, db.MarkPersonalPublicationSubmitted(run, 42))
	p, err := db.GetPersonalPublication(run)
	require.NoError(t, err)
	require.NotNil(t, p)
	assert.Equal(t, "preview", p.Phase)
	assert.Zero(t, p.ReviewID)
}
