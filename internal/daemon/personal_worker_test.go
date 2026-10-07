package daemon

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/storage"
)

func TestPersonalWorkerUsesOnlyFrozenBackup(t *testing.T) {
	raw, err := json.Marshal(ciACPExecutionConfig{Personal: &config.PersonalReviewConfig{Enabled: true, Reviewer: "alice"}})
	require.NoError(t, err)
	job := &storage.ReviewJob{Source: storage.JobSourceCI, JobType: storage.JobTypeRange, PanelMemberConfigJSON: string(raw)}
	// No config getter is needed: a missing frozen backup must not cause the
	// worker to consult repository or subsequently edited global configuration.
	wp := &WorkerPool{}
	assert := assert.New(t)
	assert.Empty(wp.resolveBackupAgent(job))
	assert.Empty(wp.resolveBackupModel(job))
	job.BackupAgent = "test"
	job.BackupModel = "frozen-model"
	assert.Equal("test", wp.resolveBackupAgent(job))
	assert.Equal("frozen-model", wp.resolveBackupModel(job))
}
