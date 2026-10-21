package cursorcloud

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// largeBody is past the 8 MiB cap this client used to have.
const largeBody = 9 << 20

func TestLargeErrorBodyKeepsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.Copy(w, io.LimitReader(repeatByte('x'), largeBody))
	}))
	t.Cleanup(srv.Close)
	client := NewHTTPClient("test-key", WithBaseURL(srv.URL))
	_, err := client.GetAgent(context.Background(), "bc_1")
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusBadGateway, apiErr.StatusCode)
	assert.Len(t, apiErr.Message, maxErrorMessageBytes)
}

func TestLargeSuccessBodyIsNotCapped(t *testing.T) {
	text := strings.Repeat("r", largeBody)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"messages": []map[string]string{
			{"type": "assistant_message", "text": text},
		}})
	}))
	t.Cleanup(srv.Close)
	client := NewHTTPClient("test-key", WithBaseURL(srv.URL))
	got, err := client.FinalMessage(context.Background(), "bc_1")
	require.NoError(t, err)
	assert.Len(t, got, largeBody)
}

func TestErrorBodyRedactsSecretStraddlingTruncation(t *testing.T) {
	const partial = "xxSECRETKEY-"
	key := partial + "TAILVALUE"
	body := strings.Repeat("x", maxErrorMessageBytes-len(partial)) + key
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	client := NewHTTPClient(key, WithBaseURL(srv.URL))
	_, err := client.GetAgent(context.Background(), "bc_1")
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.NotContains(t, apiErr.Message, partial)
	assert.NotContains(t, apiErr.Message, key)
	assert.NotContains(t, err.Error(), partial)
	assert.Contains(t, apiErr.Message, "[REDACTED]")
	assert.LessOrEqual(t, len(apiErr.Message), maxErrorMessageBytes)
}

func TestHTTPRequestDeadlineCoversHeadersAndBody(t *testing.T) {
	// A socket read is not a durable block, so a synctest bubble cannot
	// advance this deadline while the client waits on the network.
	const limit = 200 * time.Millisecond
	for name, handler := range map[string]http.HandlerFunc{
		"headers": func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		},
		"body": func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"id":`)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(handler)
			t.Cleanup(srv.Close)
			client := NewHTTPClient("k", WithBaseURL(srv.URL), WithHTTPClient(&http.Client{})).(*httpClient)
			client.requestTimeout = limit
			errc := make(chan error, 1)
			go func() {
				_, err := client.GetAgent(context.Background(), "bc_1")
				errc <- err
			}()
			var err error
			select {
			case err = <-errc:
			case <-time.After(2 * time.Second):
			}
			require.ErrorIs(t, err, context.DeadlineExceeded)
		})
	}
}

func TestWithHTTPClientDoesNotFollowRedirects(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, "/v0/agents/elsewhere", http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	hc := &http.Client{}
	client := NewHTTPClient("k", WithBaseURL(srv.URL), WithHTTPClient(hc))
	_, err := client.GetAgent(context.Background(), "bc_1")
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusFound, apiErr.StatusCode)
	assert.Equal(t, 1, hits)
	assert.Nil(t, hc.CheckRedirect, "caller's client is not mutated")
}

type repeatByte byte

func (b repeatByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}
