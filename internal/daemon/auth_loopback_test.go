package daemon

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLoopbackHostCheck(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	status := func(h http.Handler, host string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
		req.Host = host
		req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 7373}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	plain := withLoopbackHost(ok, "", nil)
	assert.Equal(t, http.StatusNoContent, status(plain, "127.0.0.1:7373"))
	assert.Equal(t, http.StatusForbidden, status(plain, "box.tail1234.ts.net"))

	proxied := withLoopbackHost(ok, "", []string{"Box.tail1234.ts.net"})
	assert.Equal(t, http.StatusNoContent, status(proxied, "box.tail1234.ts.net"))
	assert.Equal(t, http.StatusNoContent, status(proxied, "box.tail1234.ts.net:443"))
	assert.Equal(t, http.StatusForbidden, status(proxied, "evil.example.com"))

	keyed := withLoopbackHost(ok, "k", nil)
	assert.Equal(t, http.StatusNoContent, status(keyed, "box.tail1234.ts.net"))
}
