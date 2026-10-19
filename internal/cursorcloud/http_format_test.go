package cursorcloud

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFmtNeverPrintsAPIKey(t *testing.T) {
	const key = "cursor-test-secret-value-not-a-real-key"
	client := NewHTTPClient(key, WithBaseURL("https://api.example.test"))
	concrete, ok := client.(*httpClient)
	require.True(t, ok)

	for _, tc := range []struct {
		name  string
		value any
	}{
		{name: "client", value: client},
		{name: "struct", value: concrete},
		{name: "key", value: concrete.apiKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
				printed := fmt.Sprintf(verb, tc.value)
				assert.NotContains(t, printed, key, verb)
				assert.NotContains(t, printed, "cursor-test-secret", verb)
			}
		})
	}
}
