package cursorcloud

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultResponseHeaderTimeout(t *testing.T) {
	c, ok := NewHTTPClient("k").(*httpClient)
	require.True(t, ok)
	tr, ok := c.hc.Transport.(*http.Transport)
	require.True(t, ok)
	assert.Equal(t, defaultResponseHeaderTimeout, tr.ResponseHeaderTimeout)
}

func TestErrorBodyIsTruncated(t *testing.T) {
	body := strings.Repeat("x", maxErrorMessageBytes+50)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	client := NewHTTPClient("k", WithBaseURL(srv.URL))
	_, err := client.GetAgent(context.Background(), "bc_1")
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Len(t, apiErr.Message, maxErrorMessageBytes)
	assert.NotContains(t, apiErr.Message, "k")
}

func TestErrorBodyRedactsKeyStraddlingTruncation(t *testing.T) {
	const key = "straddle-secret-key-value"
	head := maxErrorMessageBytes - len(key)/2
	body := strings.Repeat("a", head) + key + strings.Repeat("b", 64)
	require.Greater(t, len(body), maxErrorMessageBytes)
	require.Contains(t, body[:maxErrorMessageBytes], key[:8])

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	client := NewHTTPClient(key, WithBaseURL(srv.URL))
	_, err := client.GetAgent(context.Background(), "bc_1")
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.NotContains(t, apiErr.Message, key)
	assert.NotContains(t, apiErr.Message, key[:8])
	assert.LessOrEqual(t, len(apiErr.Message), maxErrorMessageBytes)
}

func TestHTTPBodyStallAfterHeadersTimesOut(t *testing.T) {
	old := responseBodyTimeout
	responseBodyTimeout = 200 * time.Millisecond
	t.Cleanup(func() { responseBodyTimeout = old })

	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	errc := make(chan error, 1)
	go func() {
		client := NewHTTPClient("k", WithBaseURL(srv.URL))
		_, err := client.GetAgent(context.Background(), "bc_1")
		errc <- err
	}()
	select {
	case err := <-errc:
		require.ErrorContains(t, err, "response body deadline exceeded")
	case <-time.After(2 * time.Second):
		t.Fatal("GetAgent blocked after response headers")
	}
	<-started
}

func TestHTTPOversizedBodyErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(w, io.LimitReader(repeatByte('x'), maxResponseBytes+1))
	}))
	t.Cleanup(srv.Close)
	client := NewHTTPClient("k", WithBaseURL(srv.URL))
	_, err := client.GetAgent(context.Background(), "bc_1")
	require.ErrorContains(t, err, "response body exceeds")
}

type repeatByte byte

func (b repeatByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}
