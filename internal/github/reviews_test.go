package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuthenticatedPersonalReviewer(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"human", `{"login":"alice","type":"User"}`, true},
		{"bot", `{"login":"reviewer[bot]","type":"Bot"}`, false},
		{"missing login", `{"type":"User"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/api/v3/user", r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			client, err := NewClient("", WithBaseURL(server.URL+"/"))
			require.NoError(t, err)
			login, err := client.AuthenticatedLogin(t.Context())
			if tc.valid {
				require.NoError(t, err)
				assert.Equal(t, "alice", login)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestFormalReviewSubmissionPinsActorCommitAndEvent(t *testing.T) {
	for _, event := range []string{"APPROVE", "REQUEST_CHANGES", "COMMENT"} {
		t.Run(event, func(t *testing.T) {
			submission := ReviewSubmission{Actor: "alice", HeadSHA: "reviewed-sha", Event: event, Body: "Review result.", Marker: "<!-- operation:run -->"}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert := assert.New(t)
				assert.Equal(http.MethodPost, r.Method)
				assert.Equal("/api/v3/repos/acme/api/pulls/1/reviews", r.URL.Path)
				var payload map[string]string
				assert.NoError(json.NewDecoder(r.Body).Decode(&payload))
				assert.Equal(event, payload["event"])
				assert.Equal(submission.HeadSHA, payload["commit_id"])
				assert.Contains(payload["body"], submission.Marker)
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(json.NewEncoder(w).Encode(map[string]any{"id": 42, "user": map[string]string{"login": "alice"}, "commit_id": submission.HeadSHA, "state": reviewState(event)}))
			}))
			defer server.Close()
			client, err := NewClient("", WithBaseURL(server.URL+"/"))
			require.NoError(t, err)
			id, err := client.SubmitReview(t.Context(), "acme/api", 1, submission)
			require.NoError(t, err)
			assert.Equal(t, int64(42), id)
		})
	}
}

func TestReconcileFormalReviewFollowsPagesAndIgnoresOtherActor(t *testing.T) {
	submission := ReviewSubmission{Actor: "alice", HeadSHA: "sha", Event: "APPROVE", Marker: "<!-- operation:run -->"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", fmt.Sprintf("<http://%s%s?page=2>; rel=\"next\"", r.Host, r.URL.Path))
			fmt.Fprint(w, `[{"id":1,"user":{"login":"other"},"body":"<!-- operation:run -->","commit_id":"sha","state":"APPROVED"}]`)
		} else {
			fmt.Fprint(w, `[{"id":42,"user":{"login":"Alice"},"body":"<!-- operation:run -->","commit_id":"sha","state":"APPROVED"}]`)
		}
	}))
	defer server.Close()
	client, err := NewClient("", WithBaseURL(server.URL+"/"))
	require.NoError(t, err)
	id, err := client.FindSubmittedReview(t.Context(), "acme/api", 1, submission)
	require.NoError(t, err)
	assert.Equal(t, int64(42), id)
}

func TestRequestedReviewerMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := `{"number":1,"draft":true,"requested_reviewers":[{"login":"alice"}],"requested_teams":[{"slug":"reviewers"}]}`
		if r.URL.Path == "/api/v3/repos/acme/api/pulls" {
			body = "[" + body + "]"
		}
		fmt.Fprint(w, body)
	}))
	defer server.Close()
	client, err := NewClient("", WithBaseURL(server.URL+"/"))
	require.NoError(t, err)
	prs, err := client.ListOpenPullRequests(t.Context(), "acme/api")
	require.NoError(t, err)
	require.Len(t, prs, 1)
	pr, err := client.GetPullRequest(t.Context(), "acme/api", 1)
	require.NoError(t, err)
	assert := assert.New(t)
	assert.True(prs[0].Draft)
	assert.Equal([]string{"alice"}, prs[0].RequestedReviewers)
	assert.True(pr.Draft)
	assert.Equal([]string{"alice"}, pr.RequestedReviewers)
}
