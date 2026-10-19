package cursorcloud

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
