package daemon

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log"
	"strings"

	"go.kenn.io/roborev/internal/config"
	ghpkg "go.kenn.io/roborev/internal/github"
	reviewpkg "go.kenn.io/roborev/internal/review"
	"go.kenn.io/roborev/internal/storage"
	"go.kenn.io/roborev/pkg/structuredreview"
)

func personalRequestEligible(policy config.PersonalReviewConfig, pr ghPR) bool {
	if strings.TrimSpace(policy.Reviewer) == "" || pr.Draft || strings.EqualFold(pr.Author.Login, policy.Reviewer) {
		return false
	}
	for _, login := range pr.RequestedReviewers {
		if strings.EqualFold(login, policy.Reviewer) {
			return true
		}
	}
	return false
}

func personalRepoAllowed(cfg *config.Config, repo string) bool {
	for _, allowed := range cfg.CI.Repos {
		if strings.EqualFold(allowed, repo) {
			return true
		}
	}
	return false
}

func personalPolicySnapshot(cfg *config.Config) *config.PersonalReviewConfig {
	if !cfg.CI.Personal.Enabled {
		return nil
	}
	snapshot := cfg.CI.Personal
	return &snapshot
}

func personalJobSnapshot(job *storage.ReviewJob) (ciACPExecutionConfig, error) {
	var snapshot ciACPExecutionConfig
	if job == nil || !job.IsCIReview() || job.PanelMemberConfigJSON == "" {
		return snapshot, nil
	}
	err := json.Unmarshal([]byte(job.PanelMemberConfigJSON), &snapshot)
	return snapshot, err
}

func personalJobPolicy(job *storage.ReviewJob) (*config.PersonalReviewConfig, error) {
	snapshot, err := personalJobSnapshot(job)
	return snapshot.Personal, err
}

// Preserve repository context but never its personal-review execution policy.
func personalPromptRepoConfig(repo *config.RepoConfig, cfg *config.Config) *config.RepoConfig {
	result := config.RepoConfig{}
	if repo != nil {
		result = *repo
	}
	result.Review = cfg.Review
	result.ACP = cfg.ACP
	result.CI.Panel = cfg.CI.Personal.Panel
	return &result
}

func (p *CIPoller) personalActor(ctx context.Context, repo string) (string, error) {
	if p.personalActorFn != nil {
		return p.personalActorFn(ctx, repo)
	}
	if p.tokenProvider != nil || p.cfgGetter.Config().CI.GitHubAppConfigured() {
		return "", fmt.Errorf("personal publishing cannot fall back from GitHub App authentication")
	}
	client, err := p.githubClientForRepo(repo)
	if err != nil {
		return "", err
	}
	return client.AuthenticatedLogin(ctx)
}

func (p *CIPoller) verifyPersonalActor(ctx context.Context, repo, expected string) error {
	actual, err := p.personalActor(ctx, repo)
	if err != nil {
		return err
	}
	if expected == "" || !strings.EqualFold(actual, expected) {
		return fmt.Errorf("publishing user does not match ci.personal.reviewer")
	}
	return nil
}

// personalReviewEvent does not equate roborev's severity-derived pass with a
// human approval. Every voting member and the authoritative result must give
// an explicit pass with no findings; a partial panel can only comment.
func personalReviewEvent(policy config.PersonalReviewConfig, doc structuredreview.Document, members []storage.BatchReviewResult) string {
	if policy.RequestChanges && doc.Verdict == structuredreview.VerdictFail && len(doc.Findings) > 0 {
		return "REQUEST_CHANGES"
	}
	if !policy.Approve || doc.Verdict != structuredreview.VerdictPass || len(doc.Findings) != 0 || len(members) == 0 {
		return "COMMENT"
	}
	for _, member := range members {
		if member.Status != string(storage.JobStatusDone) {
			return "COMMENT"
		}
		result, err := reviewpkg.DecodeStructuredReview(member.StructuredOutput)
		if err != nil || result.SchemaVersion != structuredreview.SchemaVersion || result.Verdict != structuredreview.VerdictPass || len(result.Findings) != 0 {
			return "COMMENT"
		}
	}
	return "APPROVE"
}

// handlePersonalPanel owns formal publication, including restart recovery.
// True means the personal path handled the posting claim. A legacy panel is
// never published through the broad comment path while personal mode is on.
func (p *CIPoller) handlePersonalPanel(ctx context.Context, row *storage.CIPanel) bool {
	synth, err := p.db.GetSynthesisJob(row.PanelRunUUID)
	if err != nil {
		p.releasePanelClaim(row.ID)
		return true
	}
	execution, err := personalJobSnapshot(synth)
	if err != nil {
		p.releasePanelClaim(row.ID)
		return true
	}
	policy := execution.Personal
	current := p.cfgGetter.Config().CI.Personal
	if policy == nil {
		if current.Enabled {
			p.retirePanelAndDeleteAttempt(row, "legacy panel in personal mode")
			return true
		}
		return false
	}
	fail := func(err error) {
		log.Printf("personal review: panel %s not published: %v", row.PanelRunUUID, err)
		p.releasePanelClaim(row.ID)
	}
	// Identity verification also applies to reconciling an already-sent review.
	// Policy disabling stops all new writes, but never changes the frozen intent.
	if err := p.verifyPersonalActor(ctx, row.GithubRepo, policy.Reviewer); err != nil {
		fail(err)
		return true
	}
	publication, err := p.db.GetPersonalPublication(row.PanelRunUUID)
	if err != nil {
		fail(err)
		return true
	}
	if publication != nil {
		if publication.Phase == "preview" || publication.Phase == "submitted" {
			p.finishPersonalPanel(row, publication.Phase == "submitted")
			return true
		}
		var submission ghpkg.ReviewSubmission
		if err := json.Unmarshal([]byte(publication.SubmissionJSON), &submission); err != nil {
			fail(err)
			return true
		}
		id, err := p.findPersonalReview(ctx, row, submission)
		if err != nil {
			fail(err)
			return true
		}
		if id == 0 {
			fail(fmt.Errorf("submission is uncertain; no automatic resend (inspect the remote review and local publication record)"))
			return true
		}
		if err := p.db.MarkPersonalPublicationSubmitted(row.PanelRunUUID, id); err != nil {
			fail(err)
			return true
		}
		p.finishPersonalPanel(row, true)
		return true
	}
	if !p.cfgGetter.Config().CI.Enabled || !policy.Enabled || !current.Enabled || !strings.EqualFold(current.Reviewer, policy.Reviewer) || !personalRepoAllowed(p.cfgGetter.Config(), row.GithubRepo) {
		p.retirePanelAndDeleteAttempt(row, "personal review authority disabled or changed")
		return true
	}
	// Disabling permissions immediately revokes authority; enabling them cannot
	// upgrade the frozen policy of an already-queued job.
	effective := *policy
	effective.Approve = policy.Approve && current.Approve
	effective.RequestChanges = policy.RequestChanges && current.RequestChanges
	if current.ResolvedMode() != "publish" {
		effective.Mode = "dry-run"
	}
	members, err := p.db.GetPanelMemberReviews(row.PanelRunUUID)
	if err != nil {
		fail(err)
		return true
	}
	effective.Approve = effective.Approve && execution.PersonalMemberCount > 0 && execution.PersonalMemberCount == len(members)
	if synth == nil || synth.Status != storage.JobStatusDone {
		p.finishPersonalPanel(row, false)
		return true
	}
	result, err := p.db.GetReviewByJobID(synth.ID)
	if err != nil {
		fail(err)
		return true
	}
	if result == nil {
		fail(fmt.Errorf("authoritative review is missing"))
		return true
	}
	raw, err := json.Marshal(result.StructuredOutput)
	if err != nil {
		fail(err)
		return true
	}
	doc, err := reviewpkg.DecodeStructuredReview(raw)
	if err != nil || doc.SchemaVersion != structuredreview.SchemaVersion || doc.UnableToReview() || (doc.Verdict == structuredreview.VerdictFail && len(doc.Findings) == 0) {
		fail(fmt.Errorf("authoritative review is not a valid current structured result"))
		return true
	}
	// Personal review verdicts never use the quiet-hours stale-comment exception.
	target, err := p.callPanelPostTarget(ctx, row.GithubRepo, row.PRNumber)
	if err != nil {
		fail(err)
		return true
	}
	eligible := personalRequestEligible(effective, ghPR{Draft: target.Draft, Author: ghPRAuthor{Login: target.AuthorLogin}, RequestedReviewers: target.RequestedReviewers})
	_, skipped := matchingCISkipLabel(target.Labels, p.cfgGetter.Config().CI.SkipLabels)
	if !target.Open || target.HeadSHA == "" || target.HeadSHA != row.HeadSHA || !eligible || skipped {
		p.retirePanelAndDeleteAttempt(row, "personal review target is no longer eligible")
		return true
	}
	match, err := p.panelRunMatchesTargetRepo(row)
	if err != nil {
		fail(err)
		return true
	}
	if !match {
		p.retirePanelAndDeleteAttempt(row, "personal review repository mismatch")
		return true
	}
	submission := ghpkg.ReviewSubmission{
		Actor: policy.Reviewer, HeadSHA: row.HeadSHA,
		Event:  personalReviewEvent(effective, doc, members),
		Body:   "Automated review on behalf of the authenticated reviewer.\n\n" + doc.Markdown(""),
		Marker: fmt.Sprintf("<!-- roborev-personal-review:%s -->", row.PanelRunUUID),
	}
	encoded, err := json.Marshal(submission)
	if err != nil {
		fail(err)
		return true
	}
	preview := effective.ResolvedMode() != "publish"
	reserved, err := p.db.ReservePersonalPublication(row.PanelRunUUID, string(encoded), preview)
	if err != nil {
		fail(err)
		return true
	}
	if !reserved {
		p.releasePanelClaim(row.ID)
		return true
	}
	if preview {
		log.Printf("personal review: dry-run panel %s proposes %s at %s", row.PanelRunUUID, submission.Event, row.HeadSHA)
		p.finishPersonalPanel(row, false)
		return true
	}
	id, err := p.submitPersonalReview(ctx, row, submission)
	if err != nil {
		if markErr := p.db.MarkPersonalPublicationUncertain(row.PanelRunUUID); markErr != nil {
			log.Printf("personal review: preserve uncertain submission: %v", markErr)
		}
		fail(err)
		return true
	}
	if err := p.db.MarkPersonalPublicationSubmitted(row.PanelRunUUID, id); err != nil {
		fail(err)
		return true
	}
	p.finishPersonalPanel(row, true)
	return true
}

func (p *CIPoller) finishPersonalPanel(row *storage.CIPanel, submitted bool) {
	outcome := storage.PanelOutcomeNoReviewPosted
	if submitted {
		outcome = storage.PanelOutcomeReviewPosted
	}
	// Personal mode writes only a formal review, never commit statuses/checks.
	if err := p.db.MarkPanelPosted(row.ID, outcome, "", ""); err != nil {
		p.releasePanelClaim(row.ID)
	}
}

func (p *CIPoller) findPersonalReview(ctx context.Context, row *storage.CIPanel, submission ghpkg.ReviewSubmission) (int64, error) {
	if p.findPersonalReviewFn != nil {
		return p.findPersonalReviewFn(ctx, row.GithubRepo, row.PRNumber, submission)
	}
	client, err := p.githubClientForRepo(row.GithubRepo)
	if err != nil {
		return 0, err
	}
	return client.FindSubmittedReview(ctx, row.GithubRepo, row.PRNumber, submission)
}

func (p *CIPoller) submitPersonalReview(ctx context.Context, row *storage.CIPanel, submission ghpkg.ReviewSubmission) (int64, error) {
	if p.submitPersonalReviewFn != nil {
		return p.submitPersonalReviewFn(ctx, row.GithubRepo, row.PRNumber, submission)
	}
	client, err := p.githubClientForRepo(row.GithubRepo)
	if err != nil {
		return 0, err
	}
	// Reverify the actual client immediately before sending, not just the token
	// resolved during earlier eligibility checks.
	actor, err := client.AuthenticatedLogin(ctx)
	if err != nil {
		return 0, err
	}
	if !strings.EqualFold(actor, submission.Actor) {
		return 0, fmt.Errorf("publishing identity changed before submission")
	}
	pr, err := client.GetPullRequest(ctx, row.GithubRepo, row.PRNumber)
	if err != nil {
		return 0, err
	}
	eligible := personalRequestEligible(config.PersonalReviewConfig{Reviewer: submission.Actor}, ghPR{Draft: pr.Draft, Author: ghPRAuthor{Login: pr.AuthorLogin}, RequestedReviewers: pr.RequestedReviewers})
	if pr.State != "open" || pr.HeadRefOID != submission.HeadSHA || !eligible {
		return 0, fmt.Errorf("personal review target changed before submission")
	}
	current := p.cfgGetter.Config()
	_, skipped := matchingCISkipLabel(pr.Labels, current.CI.SkipLabels)
	policy := current.CI.Personal
	if skipped || !current.CI.Enabled || !policy.Enabled || policy.ResolvedMode() != "publish" || !strings.EqualFold(policy.Reviewer, submission.Actor) || !personalRepoAllowed(current, row.GithubRepo) || (submission.Event == "APPROVE" && !policy.Approve) || (submission.Event == "REQUEST_CHANGES" && !policy.RequestChanges) {
		return 0, fmt.Errorf("personal publication authority changed before submission")
	}
	return client.SubmitReview(ctx, row.GithubRepo, row.PRNumber, submission)
}
