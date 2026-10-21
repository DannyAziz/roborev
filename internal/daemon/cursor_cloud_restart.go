package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"go.kenn.io/roborev/internal/agent"
	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/cursorcloud"
	"go.kenn.io/roborev/internal/git"
	"go.kenn.io/roborev/internal/storage"
)

// startCloudLaunchRecovery reads the launches a previous run left uncleaned
// and sweeps them in the background, so startup does not wait on the
// network. The rows are read before workers start, so a launch this run
// creates is never swept.
func (s *Server) startCloudLaunchRecovery(ctx context.Context) {
	rows, err := s.db.ListUncleanedCloudLaunches()
	if err != nil {
		log.Printf("cursor-cloud: list interrupted launches: %v", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	client := s.cloudLaunchClient
	if client == nil {
		client, _ = cursorcloud.NewFromEnv()
	}
	go func() {
		if err := recoverCloudLaunches(ctx, s.db, client, rows); err != nil {
			log.Printf("cursor-cloud: interrupted launch cleanup will retry on next start: %v", err)
		}
	}()
}

// recoverCloudLaunches cleans launches a previous run did not finish. An
// agent id whose delete was not confirmed is stopped and deleted. Then each
// repository is listed once and every VM carrying one of its launch
// markers is deleted. A row is marked cleaned when that sweep finished and:
//   - its agent id was confirmed deleted, or
//   - the sweep found and deleted its VM, or
//   - it has no agent id (an ambiguous create), its VM is not listed, and
//     the launch is older than the reconcile window the create already
//     waited for a listing.
func recoverCloudLaunches(ctx context.Context, db *storage.DB, client cursorcloud.Client, rows []storage.CloudLaunch) error {
	if client == nil {
		return fmt.Errorf("cursor-cloud: %d interrupted launch(es) have no client", len(rows))
	}
	machineID, err := db.GetMachineID()
	if err != nil {
		return err
	}
	daemonID := machineID.String() //nolint:forbidigo // launch marker is prompt text
	var errs []error
	byRepo := map[string][]storage.CloudLaunch{}
	var repos []string
	for _, row := range rows {
		if row.AgentID != "" && !row.AgentDeleted {
			log.Printf("cursor-cloud: stop interrupted agent %s for job %d", row.AgentID, row.JobID)
			if err := agent.StopAndDeleteCursorCloudAgent(client, row.AgentID); err != nil {
				errs = append(errs, fmt.Errorf("launch %d: %w", row.ID, err))
				continue
			}
			if err := db.MarkCloudAgentDeleted(row.JobID, row.Token); err != nil {
				errs = append(errs, err)
			}
			row.AgentDeleted = true
		}
		repoURL := row.RepoIdentity
		if !agent.CursorCloudRepoUsable(repoURL) {
			repoURL = config.StripURLCredentials(git.GetRemoteURL(row.RepoPath, ""))
		}
		if row.Token == "" || !agent.CursorCloudRepoUsable(repoURL) {
			// No marker or no repository to list, so a duplicate VM cannot
			// be ruled out. The row stays uncleaned and keeps its warning.
			errs = append(errs, fmt.Errorf("cursor-cloud: launch %d has no marker sweep", row.ID))
			continue
		}
		if _, ok := byRepo[repoURL]; !ok {
			repos = append(repos, repoURL)
		}
		byRepo[repoURL] = append(byRepo[repoURL], row)
	}
	now := time.Now()
	for _, repoURL := range repos {
		group := byRepo[repoURL]
		tokens := make([]string, len(group))
		for i, row := range group {
			tokens[i] = row.Token
		}
		found, err := agent.SweepCursorCloudLaunches(ctx, client, daemonID, repoURL, tokens)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, row := range group {
			if !row.AgentDeleted && !found[row.Token] && !cloudLaunchPastReconcile(row.CreatedAt, now) {
				continue
			}
			if err := db.MarkCloudLaunchCleaned(row.ID); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func cloudLaunchPastReconcile(createdAt string, now time.Time) bool {
	created, err := time.ParseInLocation("2006-01-02 15:04:05", createdAt, time.UTC)
	if err != nil {
		if created, err = time.Parse(time.RFC3339, createdAt); err != nil {
			return false
		}
	}
	return !now.Before(created.Add(agent.CursorCloudReconcileWindow()))
}
