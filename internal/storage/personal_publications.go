package storage

import (
	"database/sql"
	"errors"
	"strings"
	"uuid"
)

// ObservePersonalRequest records sampled request state. An observed removal
// followed by a new request re-arms a completed review at the same SHA,
// including failed/skipped runs that have no publication intent. It preserves
// history and never retires an in-flight or uncertain publication.
func (db *DB) ObservePersonalRequest(repo string, number int, reviewer string, requested bool) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	reviewer = strings.ToLower(reviewer)
	var previous bool
	err = tx.QueryRow(`SELECT requested FROM personal_review_requests WHERE github_repo=? AND pr_number=? AND reviewer=?`, repo, number, reviewer).Scan(&previous)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	renewed := err == nil && !previous && requested
	_, err = tx.Exec(`INSERT INTO personal_review_requests(github_repo, pr_number, reviewer, requested) VALUES (?, ?, ?, ?) ON CONFLICT(github_repo, pr_number, reviewer) DO UPDATE SET requested=excluded.requested`, repo, number, reviewer, requested)
	if err != nil {
		return err
	}
	if renewed {
		_, err = tx.Exec(`UPDATE ci_pr_panels SET retired_at=datetime('now')
   WHERE github_repo=? AND pr_number=? AND posted_at IS NOT NULL AND retired_at IS NULL
   AND NOT EXISTS (SELECT 1 FROM personal_review_publications publication
    WHERE publication.panel_run_uuid=ci_pr_panels.panel_run_uuid AND publication.phase IN ('sending','uncertain'))`, repo, number)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`DELETE FROM ci_pr_review_attempts WHERE github_repo=? AND pr_number=? AND state='done'
   AND NOT EXISTS (SELECT 1 FROM ci_pr_panels p WHERE p.github_repo=ci_pr_review_attempts.github_repo AND p.pr_number=ci_pr_review_attempts.pr_number AND p.head_sha=ci_pr_review_attempts.head_sha AND p.retired_at IS NULL)`, repo, number)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PersonalPublication is a durable immutable intent, local to one daemon.
// Sending/uncertain rows must be reconciled, never automatically resent.
type PersonalPublication struct {
	SubmissionJSON string
	Phase          string
	ReviewID       int64
}

func (db *DB) GetPersonalPublication(run uuid.UUID) (*PersonalPublication, error) {
	var p PersonalPublication
	err := db.QueryRow(`SELECT submission_json, phase, review_id FROM personal_review_publications WHERE panel_run_uuid = ?`, run).Scan(&p.SubmissionJSON, &p.Phase, &p.ReviewID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ReservePersonalPublication authorizes exactly one local caller to submit.
// Rows are not overwritten when the caller loses the insertion race.
func (db *DB) ReservePersonalPublication(run uuid.UUID, submission string, preview bool) (bool, error) {
	phase := "sending"
	if preview {
		phase = "preview"
	}
	result, err := db.Exec(`INSERT INTO personal_review_publications(panel_run_uuid, submission_json, phase) VALUES (?, ?, ?) ON CONFLICT(panel_run_uuid) DO NOTHING`, run, submission, phase)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func (db *DB) MarkPersonalPublicationUncertain(run uuid.UUID) error {
	_, err := db.Exec(`UPDATE personal_review_publications SET phase = 'uncertain' WHERE panel_run_uuid = ? AND phase = 'sending'`, run)
	return err
}

func (db *DB) MarkPersonalPublicationSubmitted(run uuid.UUID, id int64) error {
	_, err := db.Exec(`UPDATE personal_review_publications SET phase = 'submitted', review_id = ? WHERE panel_run_uuid = ? AND phase IN ('sending', 'uncertain')`, id, run)
	return err
}
