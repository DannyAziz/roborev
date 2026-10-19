package cursorcloud

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
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
	assert.Equal(t, defaultRequestTimeout, c.requestTimeout)
	assert.Equal(t, defaultResponseBodyTimeout, c.responseBodyTimeout)
	assert.Equal(t, defaultResponseHeaderTimeout, c.requestTimeout)
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

func TestHTTPBodyStallAfterHeadersTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const bodyLimit = 30 * time.Second
		release := make(chan struct{})
		body := &stallBody{release: release, unblockOnClose: true}
		client := clientWithStallBody(body, time.Hour, bodyLimit)
		errc := make(chan error, 1)
		go func() {
			_, err := client.GetAgent(context.Background(), "bc_1")
			errc <- err
		}()
		synctest.Wait()
		synctest.Sleep(bodyLimit)
		requireBodyDeadline(t, errc)
	})
}

func TestHTTPRequestDeadlineCoversBodyRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const requestLimit = 45 * time.Second
		release := make(chan struct{})
		body := &stallBody{release: release, unblockOnClose: true}
		client := clientWithStallBody(body, requestLimit, time.Hour)
		errc := make(chan error, 1)
		go func() {
			_, err := client.GetAgent(context.Background(), "bc_1")
			errc <- err
		}()
		synctest.Wait()
		synctest.Sleep(requestLimit)
		requireBodyDeadline(t, errc)
	})
}

func TestHTTPBodyCloseDoesNotPinCaller(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const bodyLimit = 30 * time.Second
		release := make(chan struct{})
		body := &stallBody{release: release}
		client := clientWithStallBody(body, time.Hour, bodyLimit)
		errc := make(chan error, 1)
		go func() {
			_, err := client.GetAgent(context.Background(), "bc_1")
			errc <- err
		}()
		synctest.Wait()
		synctest.Sleep(bodyLimit)
		requireBodyDeadline(t, errc)
		// Read is still blocked. Releasing it lets the abandoned reader
		// exit so the bubble can finish.
		body.unblock()
	})
}

func TestHTTPBodyReadCancelDoesNotWaitForBodyTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		body := &stallBody{release: release, unblockOnClose: true}
		client := clientWithStallBody(body, time.Hour, time.Hour)
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() {
			_, err := client.GetAgent(ctx, "bc_1")
			errc <- err
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		var err error
		select {
		case err = <-errc:
		default:
		}
		require.ErrorIs(t, err, context.Canceled)
	})
}

func TestHTTPInjectedClientHeaderStallReturnsDeadline(t *testing.T) {
	// A socket read is not a durable block, so a synctest bubble cannot
	// advance this deadline while Client.Do waits for headers.
	const limit = 200 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
	}))
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
	case <-time.After(2 * time.Second): // socket stall; synctest cannot drive it
	}
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func requireBodyDeadline(t *testing.T, errc <-chan error) {
	t.Helper()
	var err error
	select {
	case err = <-errc:
	default:
	}
	require.ErrorContains(t, err, "response body deadline exceeded")
}

type stallBody struct {
	once           sync.Once
	release        chan struct{}
	unblockOnClose bool
}

func (b *stallBody) unblock() {
	b.once.Do(func() { close(b.release) })
}

func (b *stallBody) Read([]byte) (int, error) {
	<-b.release
	return 0, io.EOF
}

func (b *stallBody) Close() error {
	if b.unblockOnClose {
		b.unblock()
	}
	return nil
}

type limitRoundTrip func(*http.Request) (*http.Response, error)

func (f limitRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func clientWithStallBody(body io.ReadCloser, requestLimit, bodyLimit time.Duration) *httpClient {
	hc := &http.Client{Transport: limitRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        make(http.Header),
			Body:          body,
			ContentLength: -1,
		}, nil
	})}
	client := NewHTTPClient("k", WithHTTPClient(hc), WithBaseURL("https://example.com")).(*httpClient)
	client.requestTimeout = requestLimit
	client.responseBodyTimeout = bodyLimit
	return client
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
