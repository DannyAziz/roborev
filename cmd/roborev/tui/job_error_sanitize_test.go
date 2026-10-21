package tui

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/roborev/internal/storage"
)

// escapePayload is job error text as a remote API could shape it: an OSC 52
// clipboard write and a clear-screen around plain words.
const escapePayload = "launch refused\x1b]52;c;ZXZpbA==\x07\x1b[2J by cursor\x07"

// TestClassifyReasoningDetailStripsEscapes pins that the log header's
// "Detail:" line does not pass terminal escapes from job.Error through.
func TestClassifyReasoningDetailStripsEscapes(t *testing.T) {
	t.Parallel()
	job := &storage.ReviewJob{
		ID:      9,
		JobType: storage.JobTypeClassify,
		Status:  storage.JobStatusFailed,
		Source:  "auto_design",
		Error:   escapePayload,
	}
	lines := classifyReasoningLines(job, 0)
	assert.Len(t, lines, 2)
	for _, line := range lines {
		assert.NotContains(t, line, "]52;")
		assert.NotContains(t, line, "[2J")
		assert.NotContains(t, line, "\x07")
	}
	assert.Contains(t, lines[1], "Detail: launch refused by cursor")
}

// TestLogFailureFlashStripsEscapes pins that the "Job #N failed" flash shown
// when a failed job has no log does not pass terminal escapes from job.Error.
func TestLogFailureFlashStripsEscapes(t *testing.T) {
	t.Parallel()
	m := newModel(localhostEndpoint, withExternalIODisabled())
	m.currentView = viewLog
	m.logJobID = 42
	m.logFromView = viewQueue
	m.height = 30
	m.jobs = []storage.ReviewJob{
		makeJob(42, withStatus(storage.JobStatusFailed), withError(escapePayload)),
	}

	m2, _ := updateModel(t, m, logOutputMsg{err: errNoLog})

	assert.Equal(t, "Job #42 failed: launch refused by cursor", m2.flashMessage)
	assert.NotContains(t, m2.renderFlash(viewQueue), "\x1b]52;")
}
