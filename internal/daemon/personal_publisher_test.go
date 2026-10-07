package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ghpkg "go.kenn.io/roborev/internal/github"
	"go.kenn.io/roborev/internal/storage"
)

func TestPersonalPublisherRechecksActualClientAndTarget(t *testing.T) {
	for _, tc := range []struct {
		name, actor, sha string
		requested        bool
		change           func(*ciPollerHarness)
		valid            bool
	}{
		{"eligible", "alice", "sha", true, nil, true},
		{"changed token identity", "bob", "sha", true, nil, false},
		{"changed head", "alice", "new-sha", true, nil, false},
		{"removed request", "alice", "sha", false, nil, false},
		{"skip label added", "alice", "sha", true, func(h *ciPollerHarness) { h.Cfg.CI.SkipLabels = []string{"skip-auto"} }, false},
		{"CI disabled", "alice", "sha", true, func(h *ciPollerHarness) { h.Cfg.CI.Enabled = false }, false},
		{"approval revoked", "alice", "sha", true, func(h *ciPollerHarness) { h.Cfg.CI.Personal.Approve = false }, false},
		{"publish revoked", "alice", "sha", true, func(h *ciPollerHarness) { h.Cfg.CI.Personal.Mode = "dry-run" }, false},
		{"allowlist revoked", "alice", "sha", true, func(h *ciPollerHarness) { h.Cfg.CI.Repos = []string{"other/project"} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := personalHarness(t)
			h.Cfg.CI.Personal.Mode = "publish"
			h.Poller.submitPersonalReviewFn = nil
			if tc.change != nil {
				tc.change(h)
			}
			t.Setenv("GH_TOKEN", "synthetic-token")
			posts := atomic.Int64{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v3/user":
					assert.NoError(t, json.NewEncoder(w).Encode(map[string]string{"login": tc.actor, "type": "User"}))
				case r.Method == http.MethodGet && r.URL.Path == "/api/v3/repos/acme/api/pulls/1":
					reviewers := []map[string]string{}
					if tc.requested {
						reviewers = append(reviewers, map[string]string{"login": "alice"})
					}
					assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"state": "open", "head": map[string]string{"sha": tc.sha}, "user": map[string]string{"login": "author"}, "requested_reviewers": reviewers, "labels": []map[string]string{{"name": "skip-auto"}}}))
				case r.Method == http.MethodPost && r.URL.Path == "/api/v3/repos/acme/api/pulls/1/reviews":
					posts.Add(1)
					fmt.Fprint(w, `{"id":42,"state":"APPROVED","commit_id":"sha","user":{"login":"alice"}}`)
				default:
					assert.Contains(t, []string{"GET /api/v3/user", "GET /api/v3/repos/acme/api/pulls/1", "POST /api/v3/repos/acme/api/pulls/1/reviews"}, r.Method+" "+r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			h.Poller.githubAPIURL = server.URL
			submission := ghpkg.ReviewSubmission{Actor: "alice", HeadSHA: "sha", Event: "APPROVE", Body: "Synthetic review.", Marker: "<!-- synthetic-operation -->"}
			id, err := h.Poller.submitPersonalReview(t.Context(), &storage.CIPanel{GithubRepo: "acme/api", PRNumber: 1, HeadSHA: "sha"}, submission)
			if tc.valid {
				require.NoError(t, err)
				assert.Equal(t, int64(42), id)
				assert.Equal(t, int64(1), posts.Load())
			} else {
				require.Error(t, err)
				assert.Zero(t, id)
				assert.Zero(t, posts.Load())
			}
		})
	}
}
