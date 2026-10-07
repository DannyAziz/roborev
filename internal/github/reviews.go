package github

import (
	"context"
	"fmt"
	"strings"

	googlegithub "github.com/google/go-github/v91/github"
)

// ReviewSubmission identifies one immutable formal GitHub review operation.
// Marker must be unique to the local panel run, not just the PR or commit.
type ReviewSubmission struct {
	Actor   string `json:"actor"`
	HeadSHA string `json:"head_sha"`
	Event   string `json:"event"`
	Body    string `json:"body"`
	Marker  string `json:"marker"`
}

func (c *Client) AuthenticatedLogin(ctx context.Context) (string, error) {
	user, _, err := c.api.Users.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("identify publishing user: %w", err)
	}
	if user.GetType() != "User" || strings.TrimSpace(user.GetLogin()) == "" {
		return "", fmt.Errorf("personal reviews require an authenticated human user")
	}
	return user.GetLogin(), nil
}

// FindSubmittedReview reconciles a possibly accepted request after a crash or
// lost response. It never treats another actor's review as this operation.
func (c *Client) FindSubmittedReview(ctx context.Context, repo string, number int, submission ReviewSubmission) (int64, error) {
	owner, name, err := parseRepo(repo)
	if err != nil {
		return 0, err
	}
	opts := &googlegithub.ListOptions{PerPage: 100}
	for {
		reviews, response, err := c.api.PullRequests.ListReviews(ctx, owner, name, number, opts)
		if err != nil {
			return 0, fmt.Errorf("reconcile formal review: %w", err)
		}
		for _, review := range reviews {
			if !strings.EqualFold(review.GetUser().GetLogin(), submission.Actor) || !strings.Contains(review.GetBody(), submission.Marker) {
				continue
			}
			if review.GetCommitID() != submission.HeadSHA || review.GetState() != reviewState(submission.Event) {
				return 0, fmt.Errorf("operation marker matched a review with a different target or verdict")
			}
			return review.GetID(), nil
		}
		if response == nil || response.NextPage == 0 {
			return 0, nil
		}
		opts.Page = response.NextPage
	}
}

func (c *Client) SubmitReview(ctx context.Context, repo string, number int, submission ReviewSubmission) (int64, error) {
	if submission.HeadSHA == "" || submission.Actor == "" || submission.Marker == "" || strings.TrimSpace(submission.Body) == "" || reviewState(submission.Event) == "" {
		return 0, fmt.Errorf("formal review requires actor, commit, supported event, body and operation marker")
	}
	owner, name, err := parseRepo(repo)
	if err != nil {
		return 0, err
	}
	review, _, err := c.api.PullRequests.CreateReview(ctx, owner, name, number, &googlegithub.PullRequestReviewRequest{
		CommitID: ptr(submission.HeadSHA), Event: ptr(submission.Event), Body: ptr(submission.Body + "\n\n" + submission.Marker),
	})
	if err != nil {
		return 0, fmt.Errorf("submit formal review: %w", err)
	}
	if review.GetID() == 0 || !strings.EqualFold(review.GetUser().GetLogin(), submission.Actor) || review.GetCommitID() != submission.HeadSHA || review.GetState() != reviewState(submission.Event) {
		return 0, fmt.Errorf("formal review response did not confirm the intended actor, commit and event")
	}
	return review.GetID(), nil
}

func reviewState(event string) string {
	switch event {
	case "APPROVE":
		return "APPROVED"
	case "REQUEST_CHANGES":
		return "CHANGES_REQUESTED"
	case "COMMENT":
		return "COMMENTED"
	default:
		return ""
	}
}
