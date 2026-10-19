package cursorcloud

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

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

func TestHTTPOversizedBodyErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(w, io.LimitReader(repeatReader{}, maxResponseBytes+1))
	}))
	t.Cleanup(srv.Close)
	client := NewHTTPClient("k", WithBaseURL(srv.URL))
	_, err := client.GetAgent(context.Background(), "bc_1")
	require.ErrorContains(t, err, "response body exceeds")
}

func TestHTTPPlainHTTPNonLoopbackRefusedBeforeRequest(t *testing.T) {
	const key = "super-secret-cursor-key"
	called := false
	hc := &http.Client{Transport: rejectTrip(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, errors.New("should not dial")
	})}
	client := NewHTTPClient(key, WithHTTPClient(hc), WithBaseURL("http://example.com"))
	_, err := client.GetAgent(context.Background(), "bc_1")
	require.ErrorContains(t, err, "loopback")
	assert.False(t, called)
	assert.NotContains(t, err.Error(), key)

	client = NewHTTPClient(key, WithBaseURL("http://127.0.0.1:9"))
	_, err = client.GetAgent(context.Background(), "bc_1")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "loopback")
	assert.NotContains(t, err.Error(), key)
}

type repeatReader struct{}

func (repeatReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

type rejectTrip func(*http.Request) (*http.Response, error)

func (f rejectTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
