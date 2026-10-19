package cursorcloud

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFmtNeverPrintsAPIKey(t *testing.T) {
	const sentinelKey = "sentinel-cursor-api-key"
	client := NewHTTPClient(sentinelKey, WithBaseURL("https://api.example.test"))
	concrete, ok := client.(*httpClient)
	require.True(t, ok)

	type wraps struct {
		Client Client
	}
	type embeds struct {
		Client
	}
	for _, tc := range []struct {
		name  string
		value any
	}{
		{name: "client", value: client},
		{name: "pointer", value: concrete},
		{name: "struct", value: *concrete},
		{name: "key", value: concrete.apiKey},
		{name: "wraps", value: wraps{Client: client}},
		{name: "embeds", value: embeds{Client: client}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
				printed := fmt.Sprintf(verb, tc.value)
				assert.NotContains(t, printed, sentinelKey, verb)
			}
		})
	}
}
