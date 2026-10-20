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

// cloudLaunchMaxAge is how long a launch stays uncleaned before a later
// independent empty sweep may mark it cleaned, and before a token-only or
// empty-token miss is an orphan risk. A same-day restart still has to be
// able to see a VM an earlier list missed, so 24h is long enough for that
// retry and short enough that a no-POST row cannot sit pending forever.
// It is the existing documented bound, not a measured listing lag. Tests
// set the variable. An empty sweep inside the same call does not use it.
var cloudLaunchMaxAge = 24 * time.Hour

func recoverOneCloudLaunch(ctx context.Context, db *storage.DB, client cursorcloud.Client, daemonID string, row storage.CloudLaunch) error {
	if row.AgentID != "" {
		log.Printf("cursor-cloud: stop interrupted agent %s for job %d", row.AgentID, row.JobID)
		if err := agent.StopAndDeleteCursorCloudAgent(client, row.AgentID); err != nil {
			msg := fmt.Sprintf("cursor-cloud orphan risk: pending cleanup for agent %s: %v", row.AgentID, err)
			return errors.Join(err, db.NoteCloudLaunchRisk(row.JobID, msg))
		}
	}
	if row.TokenNull || row.Token == "" {
		// No marker can name a VM. One empty list deletes nothing, so it
		// does not clean. The orphan note is written now, and again after
		// the documented age even if a later start sees other agents.
		return reportCloudLaunchOrphan(db, row)
	}
	repoURL := row.RepoIdentity
	if !agent.CursorCloudRepoUsable(repoURL) {
		repoURL = config.StripURLCredentials(git.GetRemoteURL(row.RepoPath, ""))
	}
	if !agent.CursorCloudRepoUsable(repoURL) {
		// The repository cannot select a list filter. The token is unique,
		// so the marker sweep still deletes by id. One list does not clean.
		// A saved-id or token-only miss is noted once the launch is aged out.
		found, err := agent.DeleteCursorCloudLaunchByToken(ctx, client, daemonID, row.Token)
		if err != nil {
			return err
		}
		now := agent.CursorCloudNow()
		since, _ := parseCloudLaunchCreated(row.CreatedAt)
		// This call is the sweep. It is not a later empty sweep, so the gate
		// does not clean. A miss is noted. A saved-id or token-only row is
		// noted once it is aged out, including when the list was non-empty.
		if agent.SweepAllowsClean(found, row.AgentID != "", false, since, now, cloudLaunchMaxAge) {
			return db.MarkCloudLaunchCleanedByToken(row.Token)
		}
		if !found || cloudLaunchAgedOut(row.CreatedAt, now) {
			return reportCloudLaunchOrphan(db, row)
		}
		return nil
	}
	found, err := agent.DeleteCursorCloudLaunchByMarker(ctx, client, daemonID, row.Token, repoURL)
	if err != nil {
		return err
	}
	// This restart is its own call. It cleans only when the sweep deleted
	// nothing, the saved id was already removed by id, and the launch is
	// at least cloudLaunchMaxAge old. One list, or a list that deleted a
	// VM, leaves the row uncleaned with the orphan note.
	now := agent.CursorCloudNow()
	since, _ := parseCloudLaunchCreated(row.CreatedAt)
	if agent.SweepAllowsClean(found, row.AgentID != "", true, since, now, cloudLaunchMaxAge) {
		return db.MarkCloudLaunchCleanedByToken(row.Token)
	}
	if row.AgentID == "" && cloudLaunchAgedOut(row.CreatedAt, now) {
		// Aged out with or without listed agents. The note is not limited
		// to an empty list.
		return reportCloudLaunchOrphan(db, row)
	}
	if err := db.NoteCloudLaunchRisk(row.JobID, agent.CursorCloudPendingOrphanNote()); err != nil {
		return err
	}
	if found {
		return nil
	}
	return fmt.Errorf("cursor-cloud: launch %d not listed yet", row.ID)
}

func parseCloudLaunchCreated(createdAt string) (time.Time, bool) {
	created, err := time.ParseInLocation("2006-01-02 15:04:05", createdAt, time.UTC)
	if err != nil {
		if created, err = time.Parse(time.RFC3339, createdAt); err != nil {
			return time.Time{}, false
		}
	}
	return created, true
}

func cloudLaunchAgedOut(createdAt string, now time.Time) bool {
	created, ok := parseCloudLaunchCreated(createdAt)
	if !ok {
		return false
	}
	return !created.Add(cloudLaunchMaxAge).After(now)
}

func reportCloudLaunchOrphan(db *storage.DB, row storage.CloudLaunch) error {
	msg := fmt.Sprintf("cursor-cloud orphan risk: job %d launch %d was not listed within %s", row.JobID, row.ID, cloudLaunchMaxAge)
	log.Print(msg)
	return db.NoteCloudLaunchRisk(row.JobID, msg)
}
