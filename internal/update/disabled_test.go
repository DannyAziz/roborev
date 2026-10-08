package update

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDisabledUpdaterNeverChecksOrInstalls(t *testing.T) {
	updater := NewUpdater(Deps{DisableSelfUpdate: true, Executable: func() (string, error) { panic("disabled updater must not resolve an install destination") }})
	info, err := updater.CheckForUpdate(true)
	require.NoError(t, err)
	assert.Nil(t, info)
	require.ErrorContains(t, updater.PerformUpdate(nil, nil), "self-updates are disabled")
}
