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
		marker   string
		detected bool
		want     bool
	}{{"0", false, false}, {"0", true, false}, {"", false, false}, {"", true, false}, {"1", false, true}} {
		t.Run(agent.HostMarkerEnv+"="+tc.marker+"/detected="+map[bool]string{true: "yes", false: "no"}[tc.detected], func(t *testing.T) {
			t.Setenv(agent.HostMarkerEnv, tc.marker)
			t.Setenv("CURSOR_AGENT", "")
			if tc.detected {
				// Detection runs a named host but never lists it.
				t.Setenv("CURSOR_AGENT", "1")
				t.Setenv("CLOUD_AGENT_TEST_DETECTION", "1")
			}
			m := rerunPickerModel(t, rerunOKHandler)
			options, err := m.availableRerunAgents(&m.jobs[0])
			require.NoError(t, err)
			assert.Contains(t, options, "picker-alternate")
			assert.Equal(t, tc.want, slices.Contains(options, "host"), "%v", options)
		})
	}
}
