package daemon

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoopbackListenerRefusesRebindingHost sends a request to the daemon's
// loopback listener under a foreign Host, as a DNS-rebinding page would. With
// no auth_key the API is otherwise open, so the refusal is what keeps that
// page from reading or enqueueing jobs. Loopback names still work, and the
// TLS listener is unchanged.
func TestLoopbackListenerRefusesRebindingHost(t *testing.T) {
	s := newAuthTestServer(t, "")
	server := httptest.NewServer(s.httpServer.Handler)
	t.Cleanup(server.Close)
	port := mustURL(t, server.URL).Port()
	for _, tc := range []struct {
		host string
		want int
	}{
		{host: "rebind.attacker.example:" + port, want: http.StatusForbidden},
		{host: "rebind.attacker.example", want: http.StatusForbidden},
		{host: "127.0.0.1:" + port, want: http.StatusOK},
		{host: "localhost:" + port, want: http.StatusOK},
		{host: "localhost", want: http.StatusOK},
	} {
		t.Run(tc.host, func(t *testing.T) {
			assert.Equal(t, tc.want, getWithHost(t, server.Client(), server.URL+"/api/jobs", tc.host))
		})
	}

	tlsServer := httptest.NewTLSServer(s.httpServer.Handler)
	t.Cleanup(tlsServer.Close)
	// Without a client certificate the TLS listener answers 401 from the
	// auth check, not the Host refusal.
	assert.Equal(t, http.StatusUnauthorized, getWithHost(t, tlsServer.Client(), tlsServer.URL+"/api/jobs", "daemon.example"))
}

func getWithHost(t *testing.T, client *http.Client, rawURL, host string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	require.NoError(t, err)
	req.Host = host
	resp, err := client.Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	return parsed
}
