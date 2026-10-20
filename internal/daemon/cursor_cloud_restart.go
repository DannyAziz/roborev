package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"

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
	client, _ := cursorcloud.NewFromEnv()
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

func recoverOneCloudLaunch(ctx context.Context, db *storage.DB, client cursorcloud.Client, daemonID string, row storage.CloudLaunch) error {
	if row.AgentID != "" {
		log.Printf("cursor-cloud: stop interrupted agent %s for job %d", row.AgentID, row.JobID)
		if err := agent.StopAndDeleteCursorCloudAgent(client, row.AgentID); err != nil {
			return err
		}
	}
	if row.Token != "" {
		repoURL := config.StripURLCredentials(git.GetRemoteURL(row.RepoPath, ""))
		if err := agent.DeleteCursorCloudLaunchByMarker(ctx, client, daemonID, row.Token, repoURL); err != nil {
			return err
		}
	}
	return db.MarkCloudLaunchCleaned(row.ID)
}
