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
	return recoverCloudLaunches(ctx, db, client, nil)
}

func (s *Server) recoverStaleJobs(ctx context.Context) error {
	if err := s.db.ResetStaleJobs(); err != nil {
		return err
	}
	client := s.cloudLaunchClient
	if client == nil {
		client, _ = cursorcloud.NewFromEnv()
	}
	var globalCfg *config.Config
	if s.configWatcher != nil {
		globalCfg = s.configWatcher.Config()
	}
	if err := recoverCloudLaunches(ctx, s.db, client, globalCfg); err != nil {
		log.Printf("cursor-cloud: interrupted launch cleanup will retry on next start: %v", err)
	}
	return nil
}

func recoverCloudLaunches(ctx context.Context, db *storage.DB, client cursorcloud.Client, globalCfg *config.Config) error {
	rows, err := db.ListUncleanedCloudLaunches()
	if err != nil || len(rows) == 0 {
		return err
	}
	if client == nil {
		var errs []error
		for _, row := range rows {
			errs = append(errs, noteRestartClientFailure(db, row))
		}
		errs = append(errs, fmt.Errorf("cursor-cloud: %d interrupted launch(es) have no client", len(rows)))
		return errors.Join(errs...)
	}
	machineID, err := db.GetMachineID()
	if err != nil {
		return err
	}
	daemonID := machineID.String() //nolint:forbidigo // launch marker is prompt text
	var errs []error
	for _, row := range rows {
		if err := recoverOneCloudLaunch(ctx, db, client, daemonID, row, globalCfg); err != nil {
			log.Printf("cursor-cloud: cleanup job %d launch %d: %v", row.JobID, row.ID, err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// cursorCloudSamePassResweep, when set, lists again before this pass decides.
// That list is still this pass. It is not a later restart. Tests set it.
var cursorCloudSamePassResweep func(ctx context.Context, client cursorcloud.Client, daemonID, token, repoURL string) (bool, error)

func recoverOneCloudLaunch(ctx context.Context, db *storage.DB, client cursorcloud.Client, daemonID string, row storage.CloudLaunch, globalCfg *config.Config) error {
	cleanDelay := config.ResolveCloudLaunchCleanDelay(row.RepoPath, globalCfg)
	// Read before this pass lists or writes a note. A second list below
	// cannot turn this pass into the confirming sweep.
	seen, err := db.CloudLaunchSweepSeen(row.ID)
	if err != nil {
		return err
	}
	if row.AgentID != "" {
		log.Printf("cursor-cloud: stop interrupted agent %s for job %d", row.AgentID, row.JobID)
		if err := agent.StopAndDeleteCursorCloudAgent(client, row.AgentID); err != nil {
			msg := fmt.Sprintf("cursor-cloud orphan risk: pending cleanup for agent %s: %v", row.AgentID, err)
			return errors.Join(err, db.NoteCloudLaunchRisk(row.JobID, msg))
		}
		// The id delete is not a marker sweep. A VM that still carries this
		// launch's marker is handled below. This tick does not clean here.
	}
	if row.Token == "" {
		// No marker can name a VM, so this pass does not list. The gate is
		// the same one a later empty sweep uses. One pass does not clean.
		// The orphan note is written now, including when a later start sees
		// other agents and the launch is already past the delay.
		return holdEmptyTokenSweep(db, row, seen, cleanDelay)
	}
	repoURL := row.RepoIdentity
	if !agent.CursorCloudRepoUsable(repoURL) {
		repoURL = config.StripURLCredentials(git.GetRemoteURL(row.RepoPath, ""))
	}
	if !agent.CursorCloudRepoUsable(repoURL) {
		// A saved id was deleted above. Another VM from the same token is
		// only visible to the marker sweep, so the row stays uncleaned.
		// Nothing is listed or deleted by token alone, and nothing is
		// cleaned, by token or by scope.
		if row.AgentID != "" {
			return errors.Join(
				fmt.Errorf("cursor-cloud: launch %d has no repository to sweep", row.ID),
				reportCloudLaunchOrphan(db, row, cleanDelay),
			)
		}
		// Nothing to list, so this is not list lag. A token-only row can still
		// have a live VM. Surface the orphan on this start and every later
		// start. A failed note write is returned.
		return reportCloudLaunchOrphan(db, row, cleanDelay)
	}
	found, err := agent.DeleteCursorCloudLaunchByMarker(ctx, client, daemonID, row.Token, repoURL)
	if err != nil {
		if agent.CursorCloudCleanupOp(err) == "list" {
			return noteRestartListFailure(db, row, err)
		}
		return noteRestartSweepFailure(db, row, err)
	}
	if cursorCloudSamePassResweep != nil {
		again, againErr := cursorCloudSamePassResweep(ctx, client, daemonID, row.Token, repoURL)
		if againErr != nil {
			return againErr
		}
		found = found || again
	}
	return holdListedSweep(db, row, found, seen, cleanDelay)
}

func holdEmptyTokenSweep(db *storage.DB, row storage.CloudLaunch, seen bool, cleanDelay time.Duration) error {
	now := agent.CursorCloudNow()
	since, _ := parseCloudLaunchCreated(row.CreatedAt)
	if agent.SweepAllowsClean(false, row.AgentID != "", seen, since, now, cleanDelay) {
		// A blank token names no launch. Another blank row on this job has
		// its own saved id and delete result, so only this row is cleaned.
		return db.MarkCloudLaunchCleaned(row.ID)
	}
	return reportCloudLaunchOrphan(db, row, cleanDelay)
}

func holdListedSweep(db *storage.DB, row storage.CloudLaunch, found, seen bool, cleanDelay time.Duration) error {
	now := agent.CursorCloudNow()
	since, _ := parseCloudLaunchCreated(row.CreatedAt)
	if agent.SweepAllowsClean(found, row.AgentID != "", seen, since, now, cleanDelay) {
		return db.MarkCloudLaunchCleanedByToken(row.JobID, row.RepoID, row.Token)
	}
	if err := db.NoteCloudLaunchSweep(row.ID); err != nil {
		return err
	}
	if row.AgentID == "" && cloudLaunchAgedOut(row.CreatedAt, now, cleanDelay) {
		// Aged out with or without listed agents. The note is not limited
		// to an empty list.
		return reportCloudLaunchOrphan(db, row, cleanDelay)
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

func noteRestartClientFailure(db *storage.DB, row storage.CloudLaunch) error {
	msg := fmt.Sprintf("cursor-cloud orphan risk: pending cleanup: no client for launch %d", row.ID)
	log.Print(msg)
	return db.NoteCloudLaunchRisk(row.JobID, msg)
}

func noteRestartListFailure(db *storage.DB, row storage.CloudLaunch, cause error) error {
	msg := fmt.Sprintf("cursor-cloud orphan risk: pending cleanup: list: %v", cause)
	log.Print(msg)
	return errors.Join(cause, db.NoteCloudLaunchRisk(row.JobID, msg))
}

func noteRestartSweepFailure(db *storage.DB, row storage.CloudLaunch, cause error) error {
	msg := fmt.Sprintf("cursor-cloud orphan risk: pending cleanup: sweep: %v", cause)
	log.Print(msg)
	return errors.Join(cause, db.NoteCloudLaunchRisk(row.JobID, msg))
}

func cloudLaunchAgedOut(createdAt string, now time.Time, delay time.Duration) bool {
	created, ok := parseCloudLaunchCreated(createdAt)
	if !ok {
		return false
	}
	return !created.Add(delay).After(now)
}

func reportCloudLaunchOrphan(db *storage.DB, row storage.CloudLaunch, delay time.Duration) error {
	msg := fmt.Sprintf("cursor-cloud orphan risk: job %d launch %d was not listed within cloud_launch_clean_delay_hours (%s)", row.JobID, row.ID, delay)
	log.Print(msg)
	if err := db.NoteCloudLaunchRisk(row.JobID, msg); err != nil {
		return err
	}
	return nil
}
