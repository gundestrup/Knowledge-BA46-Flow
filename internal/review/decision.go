package review

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
)

// DecideCurrent checks the live source before accepting a full-file snapshot.
// Rejections remain possible even when the source is stale or inaccessible.
func DecideCurrent(ctx context.Context, c config.Config, db *database.DB, idText, status, note string) error {
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid id: %w", err)
	}
	if status == "pending" {
		return db.WithdrawAccepted(ctx, id)
	}
	if status != "accepted" && status != "rejected" {
		return fmt.Errorf("unsupported review decision %q", status)
	}
	if status == "accepted" {
		s, err := db.Suggestion(ctx, id)
		if err != nil {
			return err
		}
		if s.Status != "pending" {
			return fmt.Errorf("suggestion %d is not pending", id)
		}
		if _, err := os.Stat(c.RepositoryPath); err != nil {
			return fmt.Errorf("repository is not accessible: %w", err)
		}
		current, err := hashSource(c.RepositoryPath, s.DocumentPath)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err != nil || current != s.SourceSHA256 {
			reason := "source document changed or disappeared before acceptance; review a new proposal"
			if err := db.MarkSuperseded(ctx, id, reason); err != nil {
				return err
			}
			return errors.New(reason)
		}
		if err := validateForReview(ctx, c, db, s); err != nil {
			return fmt.Errorf("proposal is invalid and must be regenerated: %w", err)
		}
		var other int64
		err = db.QueryRowContext(ctx, `SELECT id FROM suggestions WHERE document_path=? AND status='accepted' AND id<>? ORDER BY id LIMIT 1`, s.DocumentPath, id).Scan(&other)
		if err == nil {
			return fmt.Errorf("suggestion #%d is already accepted for %s; apply or review that alternative first", other, s.DocumentPath)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return db.AcceptExclusive(ctx, id)
	}
	return db.Decide(ctx, id, status, note)
}

func InteractiveCurrent(ctx context.Context, c config.Config, db *database.DB) error {
	return interactive(ctx, db, func(ctx context.Context, id int64, status, note string) error {
		return DecideCurrent(ctx, c, db, strconv.FormatInt(id, 10), status, note)
	})
}
