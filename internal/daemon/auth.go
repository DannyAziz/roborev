package daemon

import (
	"context"
	"encoding/json/v2"
	"net"
	"net/http"
	"strings"

	"go.kenn.io/roborev/internal/auth"
)

// withAuthentication captures the startup key so a config reload or deletion
// cannot weaken authentication on an already running daemon.
func withAuthentication(next http.Handler, key string) http.Handler {
	if key == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the browser session boundary and the signed remote listener can
		// set these internal principals.
		if _, authenticated := BrowserPrincipalFromContext(r.Context()); authenticated || remoteScoped(r.Context()) {
			next.ServeHTTP(w, r)
			return
		}
		headers := r.Header.Values("Authorization")
		authorized := false
		if len(headers) == 1 {
			scheme, credential, ok := strings.Cut(headers[0], " ")
			authorized = ok && strings.EqualFold(scheme, "Bearer") && auth.EqualKey(key, credential)
		}
		if !authorized {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.MarshalWrite(w, ErrorResponse{Error: "daemon authentication required"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

type verifiedCallerKey struct{}

// callerVerified reports whether the API request presented the daemon's
// credentials. Liveness routes also answer callers without credentials and
// must not return more than identity and coarse health to them.
func callerVerified(ctx context.Context) bool {
	verified, _ := ctx.Value(verifiedCallerKey{}).(bool)
	return verified
}

// livenessRoutes answer callers that present no credentials, so a monitor or
// another account can confirm that the daemon is running.
var livenessRoutes = map[string]bool{"/api/ping": true, "/api/health": true}

// withAPIAuthentication guards the daemon API listeners. A caller is verified
// when it presents auth_key (if set) and, on the mutual TLS listener, a client
// certificate. A wrong key is always rejected so clients can report it, but a
// caller without credentials may still read the liveness routes.
func withAPIAuthentication(next http.Handler, key string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the browser session boundary and the signed remote listener can
		// set these internal principals.
		if _, authenticated := BrowserPrincipalFromContext(r.Context()); authenticated || remoteScoped(r.Context()) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), verifiedCallerKey{}, true)))
			return
		}
		headers := r.Header.Values("Authorization")
		keyOK := key == ""
		if !keyOK && len(headers) == 1 {
			scheme, credential, ok := strings.Cut(headers[0], " ")
			keyOK = ok && strings.EqualFold(scheme, "Bearer") && auth.EqualKey(key, credential)
		}
		// r.TLS is set only on the mutual TLS listener, which verifies any
		// certificate a client presents during the handshake.
		certOK := r.TLS == nil || len(r.TLS.VerifiedChains) > 0
		switch {
		case keyOK && certOK:
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), verifiedCallerKey{}, true)))
		case (keyOK || len(headers) == 0) && r.Method == http.MethodGet && livenessRoutes[r.URL.Path]:
			next.ServeHTTP(w, r)
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("WWW-Authenticate", "Bearer")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.MarshalWrite(w, ErrorResponse{Error: "daemon authentication required"})
		}
	})
}

// withLoopbackHost refuses a plain HTTP request on a loopback connection
// whose Host is not a loopback name. A web page that rebinds its own DNS
// name to 127.0.0.1 reaches this listener as same-origin and sends that name
// as Host; a local client sends localhost or a loopback address. TLS, Unix
// socket, and non-loopback connections are unchanged. The browser and
// remote listeners call the API without this check and keep their own
// Host policy. With auth_key set a rebound page cannot authenticate, so the
// check is skipped. allowedHosts are extra Host names a local reverse proxy
// (Tailscale Serve, nginx, Caddy) forwards with.
func withLoopbackHost(next http.Handler, authKey string, allowedHosts []string) http.Handler {
	if authKey != "" {
		return next
	}
	allowed := make(map[string]bool, len(allowedHosts))
	for _, host := range allowedHosts {
		allowed[strings.ToLower(strings.TrimSpace(host))] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		local, _ := r.Context().Value(http.LocalAddrContextKey).(*net.TCPAddr)
		if r.TLS == nil && local != nil && local.IP.IsLoopback() && !isLoopbackAuthority(r.Host) && !allowed[authorityHostname(r.Host)] {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.MarshalWrite(w, ErrorResponse{Error: "daemon requests must use a loopback host name"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authorityHostname is the lower-case host name of a Host header, without
// its port.
func authorityHostname(authority string) string {
	if host, _, err := net.SplitHostPort(authority); err == nil {
		authority = host
	}
	return strings.ToLower(strings.Trim(authority, "[]"))
}
