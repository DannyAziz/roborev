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

func recoverStaleJobs(ctx context.Context, db *storage.DB, client cursorcloud.Client) error {
	if err := db.ResetStaleJobs(); err != nil {
		return err
	}
	return recoverCloudLaunches(ctx, db, client)
}

func (s *Server) recoverStaleJobs(ctx context.Context) error {
	if err := s.db.ResetStaleJobs(); err != nil {
		return err
	}
	client := s.cloudLaunchClient
	if client == nil {
		client, _ = cursorcloud.NewFromEnv()
	}
	if err := recoverCloudLaunches(ctx, s.db, client); err != nil {
		log.Printf("cursor-cloud: interrupted launch cleanup will retry on next start: %v", err)
	}
	return nil
}

func recoverCloudLaunches(ctx context.Context, db *storage.DB, client cursorcloud.Client) error {
	rows, err := db.ListUncleanedCloudLaunches()
	if err != nil || len(rows) == 0 {
		return err
	}
	if client == nil {
		return fmt.Errorf("cursor-cloud: %d interrupted launch(es) have no client", len(rows))
	}
	machineID, err := db.GetMachineID()
	if err != nil {
		return err
	}
	daemonID := machineID.String() //nolint:forbidigo // launch marker is prompt text
	var errs []error
	for _, row := range rows {
		if err := recoverOneCloudLaunch(ctx, db, client, daemonID, row); err != nil {
			log.Printf("cursor-cloud: cleanup job %d launch %d: %v", row.JobID, row.ID, err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// cloudLaunchMaxAge is how long a token-only row may miss List Agents before
// that miss is an orphan risk. The list can lag the create, so a young miss
// is not a delete and not a 404: the row stays uncleaned and the next start
// sweeps it again. A crash before the POST saved a token and no VM; 24h is
// long enough that a same-day restart still retries a lagging list, and short
// enough that a no-POST row cannot sit pending forever. Past that age the row
// is still not cleaned. The risk is written to the log and the job error.
const cloudLaunchMaxAge = 24 * time.Hour

func recoverOneCloudLaunch(ctx context.Context, db *storage.DB, client cursorcloud.Client, daemonID string, row storage.CloudLaunch) error {
	if row.AgentID != "" {
		log.Printf("cursor-cloud: stop interrupted agent %s for job %d", row.AgentID, row.JobID)
		if err := agent.StopAndDeleteCursorCloudAgent(client, row.AgentID); err != nil {
			return err
		}
	}
	if row.Token == "" {
		// A known id was deleted above (nil or 404) or this list is empty.
		// Anything else stays uncleaned: there was no confirming delete or list.
		if row.AgentID == "" {
			agents, err := client.ListAgents(ctx)
			if err != nil {
				return err
			}
			if len(agents) != 0 {
				return fmt.Errorf("cursor-cloud: launch %d has an empty token and was not listed", row.ID)
			}
		}
		return db.MarkCloudLaunchCleaned(row.ID)
	}
	repoURL := row.RepoIdentity
	if !agent.CursorCloudRepoUsable(repoURL) {
		repoURL = config.StripURLCredentials(git.GetRemoteURL(row.RepoPath, ""))
	}
	if !agent.CursorCloudRepoUsable(repoURL) {
		// The saved id was deleted above when it was set. Another VM from
		// the same token is only visible to the marker sweep.
		return fmt.Errorf("cursor-cloud: launch %d has no repository to sweep", row.ID)
	}
	found, err := agent.DeleteCursorCloudLaunchByMarker(ctx, client, daemonID, row.Token, repoURL)
	if err != nil {
		return err
	}
	// A token with no saved agent id is cleaned only when this list contained
	// it. An empty list is the lag, not a confirmed delete.
	if row.AgentID == "" && !found {
		if cloudLaunchAgedOut(row.CreatedAt, time.Now().UTC()) {
			return reportCloudLaunchOrphan(db, row)
		}
		return fmt.Errorf("cursor-cloud: launch %d not listed yet", row.ID)
	}
	return db.MarkCloudLaunchCleaned(row.ID)
}

func cloudLaunchAgedOut(createdAt string, now time.Time) bool {
	created, err := time.ParseInLocation("2006-01-02 15:04:05", createdAt, time.UTC)
	if err != nil {
		if created, err = time.Parse(time.RFC3339, createdAt); err != nil {
			return false
		}
	}
	return !created.Add(cloudLaunchMaxAge).After(now)
}

func reportCloudLaunchOrphan(db *storage.DB, row storage.CloudLaunch) error {
	msg := fmt.Sprintf("cursor-cloud orphan risk: job %d launch %d was not listed within %s", row.JobID, row.ID, cloudLaunchMaxAge)
	log.Print(msg)
	return db.NoteCloudLaunchRisk(row.JobID, msg)
}
