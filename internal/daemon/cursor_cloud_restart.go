package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
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
		return fmt.Errorf("cursor-cloud: %d interrupted launch(es) have no client", len(rows))
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
	// Read before this pass writes. A clean with no persisted orphan note
	// is the saved-id miss: the row would be gone and nothing would record it.
	noted, err := persistedCleanNote(db, row.JobID)
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
	if row.TokenNull || row.Token == "" {
		// No marker can name a VM, so this pass does not list. The gate is
		// the same one a later empty sweep uses. One pass does not clean.
		// The orphan note is written now, including when a later start sees
		// other agents and the launch is already past the delay.
		return holdEmptyTokenSweep(db, row, seen, noted, cleanDelay)
	}
	repoURL := row.RepoIdentity
	if !agent.CursorCloudRepoUsable(repoURL) {
		repoURL = config.StripURLCredentials(git.GetRemoteURL(row.RepoPath, ""))
	}
	if !agent.CursorCloudRepoUsable(repoURL) {
		// The repository cannot select a list filter. The token is unique,
		// so the marker sweep still deletes by id. This list is not a later
		// pass. A saved-id or token-only miss is noted once the launch is
		// aged out, including when the list was non-empty.
		found, err := agent.DeleteCursorCloudLaunchByToken(ctx, client, daemonID, row.Token)
		if err != nil {
			// The sweep did not confirm a delete. Note that before returning.
			return errors.Join(err, noteUnconfirmedCloudDelete(db, row))
		}
		return holdListedSweep(db, row, found, seen, noted, cleanDelay, true)
	}
	found, err := agent.DeleteCursorCloudLaunchByMarker(ctx, client, daemonID, row.Token, repoURL)
	if err != nil {
		return err
	}
	if cursorCloudSamePassResweep != nil {
		again, againErr := cursorCloudSamePassResweep(ctx, client, daemonID, row.Token, repoURL)
		if againErr != nil {
			return againErr
		}
		found = found || again
	}
	return holdListedSweep(db, row, found, seen, noted, cleanDelay, false)
}

func holdEmptyTokenSweep(db *storage.DB, row storage.CloudLaunch, seen, noted bool, cleanDelay time.Duration) error {
	now := agent.CursorCloudNow()
	since, _ := parseCloudLaunchCreated(row.CreatedAt)
	if noted && agent.SweepAllowsClean(false, row.AgentID != "", seen, since, now, cleanDelay) {
		return db.MarkCloudLaunchCleanedByToken(row.Token)
	}
	return reportCloudLaunchOrphan(db, row, cleanDelay)
}

func holdListedSweep(db *storage.DB, row storage.CloudLaunch, found, seen, noted bool, cleanDelay time.Duration, unusable bool) error {
	now := agent.CursorCloudNow()
	since, _ := parseCloudLaunchCreated(row.CreatedAt)
	if noted && agent.SweepAllowsClean(found, row.AgentID != "", seen, since, now, cleanDelay) {
		return db.MarkCloudLaunchCleanedByToken(row.Token)
	}
	if err := db.NoteCloudLaunchSweep(row.ID); err != nil {
		return err
	}
	if unusable {
		if !found || cloudLaunchAgedOut(row.CreatedAt, now, cleanDelay) {
			return reportCloudLaunchOrphan(db, row, cleanDelay)
		}
		if err := db.NoteCloudLaunchRisk(row.JobID, agent.CursorCloudPendingOrphanNote()); err != nil {
			return err
		}
		return nil
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

func noteUnconfirmedCloudDelete(db *storage.DB, row storage.CloudLaunch) error {
	return db.NoteCloudLaunchRisk(row.JobID, agent.CursorCloudPendingOrphanNote())
}

// persistedCleanNote reports whether a previous pass already stored the
// orphan note on the job. A missing job has nowhere to store it. A clean
// still requires the separate sweep tick in that case.
func persistedCleanNote(db *storage.DB, jobID int64) (bool, error) {
	var text string
	err := db.QueryRow(`SELECT COALESCE(error, '') FROM review_jobs WHERE id = ?`, jobID).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return strings.Contains(text, "orphan risk"), nil
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

func cloudLaunchAgedOut(createdAt string, now time.Time, delay time.Duration) bool {
	created, ok := parseCloudLaunchCreated(createdAt)
	if !ok {
		return false
	}
	return !created.Add(delay).After(now)
}

func reportCloudLaunchOrphan(db *storage.DB, row storage.CloudLaunch, delay time.Duration) error {
	msg := fmt.Sprintf("cursor-cloud orphan risk: job %d launch %d was not listed within %s", row.JobID, row.ID, delay)
	log.Print(msg)
	return db.NoteCloudLaunchRisk(row.JobID, msg)
}
