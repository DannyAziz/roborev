package tui

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/roborev/internal/agent"
)

func TestRerunAgentPickerListsHostOnlyWithMarker(t *testing.T) { //nolint:paralleltest // global agent registry and environment
	registerRerunPickerAgent(t, "picker-current")
	registerRerunPickerAgent(t, "picker-alternate")
	for _, tc := range []struct {
		marker string
		want   bool
	}{{"0", false}, {"", false}, {"1", true}} {
		t.Run(agent.HostMarkerEnv+"="+tc.marker, func(t *testing.T) {
			t.Setenv(agent.HostMarkerEnv, tc.marker)
			m := rerunPickerModel(t, rerunOKHandler)
			options, err := m.availableRerunAgents(&m.jobs[0])
			require.NoError(t, err)
			assert.Contains(t, options, "picker-alternate")
			assert.Equal(t, tc.want, slices.Contains(options, "host"), "%v", options)
		})
	}
}
