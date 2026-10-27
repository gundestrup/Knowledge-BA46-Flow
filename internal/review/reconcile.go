package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/mcppolicy"
	"knowledge-ba46-flow/internal/proposal"
)

type Assessment struct {
	ID            int64    `json:"id"`
	DocumentPath  string   `json:"document_path"`
	DocumentID    string   `json:"document_id,omitempty"`
	Status        string   `json:"status"`
	State         string   `json:"state"`
	Reason        string   `json:"reason,omitempty"`
	CurrentSHA256 string   `json:"current_sha256,omitempty"`
	ChangeTypes   []string `json:"change_types"`
	AcceptedID    int64    `json:"accepted_id,omitempty"`
	Retire        bool     `json:"retire"`
}

type Reconciliation struct {
	Token        string         `json:"token"`
	WriteScope   string         `json:"write_scope"`
	Counts       map[string]int `json:"counts"`
	RetireCount  int            `json:"retire_count"`
	Items        []Assessment   `json:"items"`
	Retired      int            `json:"retired"`
	BatchID      string         `json:"batch_id,omitempty"`
	QueueVersion string         `json:"-"`
}

// Assess is read-only. In particular it must not use DecideCurrent, whose
// acceptance-time staleness check intentionally transitions stale rows.
func Assess(ctx context.Context, c config.Config, db *database.DB, s database.Suggestion, accepted map[string]int64) Assessment {
	a := Assessment{ID: s.ID, DocumentPath: s.DocumentPath, Status: s.Status, State: "unavailable", ChangeTypes: []string{}}
	root, err := os.OpenRoot(c.RepositoryPath)
	if err != nil {
		a.Reason = "repository is unavailable"
		return a
	}
	defer root.Close()
	original, err := readCurrentSource(root, s.DocumentPath, c.MaxDocumentBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			a.State, a.Retire = "missing", true
		}
		a.Reason = err.Error()
		return a
	}
	fields, _, _ := frontmatter.Parse(original)
	a.DocumentID, _ = fields["document_id"].(string)
	exists := func(path string) bool {
		info, err := root.Stat(path)
		return err == nil && info.Mode().IsRegular()
	}
	a.ChangeTypes = proposal.ChangeTypes(s.DocumentPath, s.Action, original, s.ProposedContent,
		func(path string) (string, bool) { return db.RetargetPath(ctx, path) }, exists)
	sum := sha256.Sum256([]byte(frontmatter.Canonical(original)))
	a.CurrentSHA256 = hex.EncodeToString(sum[:])
	if a.CurrentSHA256 != s.SourceSHA256 {
		a.State, a.Retire, a.Reason = "stale", true, "source content changed; regenerate against the current note"
		return a
	}
	targets := []string{s.DocumentPath}
	if s.TargetPath != "" {
		targets = append(targets, s.TargetPath)
	}
	if err := mcppolicy.AssertMutationAllowed(root, targets...); err != nil {
		a.State, a.Reason = "blocked", err.Error()
		return a
	}
	if err := validateForReview(ctx, c, db, s); err != nil {
		a.State, a.Reason = "invalid", err.Error()
		a.Retire = true
		if errors.Is(err, proposal.ErrWriteScope) || errors.Is(err, ErrReviewBlocked) || errors.Is(err, mcppolicy.ErrDenied) {
			a.State, a.Retire = "blocked", false
		} else if errors.Is(err, ErrValidationUnavailable) {
			a.State, a.Retire = "unavailable", false
		}
		return a
	}
	if other := accepted[s.DocumentPath]; other != 0 && other != s.ID {
		a.State, a.AcceptedID = "conflict", other
		a.Reason = fmt.Sprintf("alternative #%d is already accepted; withdraw it before choosing another", other)
		return a
	}
	a.State = "valid"
	return a
}

func PreviewReconciliation(ctx context.Context, c config.Config, db *database.DB) (Reconciliation, error) {
	p := Reconciliation{WriteScope: c.EffectiveWriteScope(), Counts: map[string]int{}, Items: []Assessment{}}
	root, err := os.OpenRoot(c.RepositoryPath)
	if err != nil {
		return p, fmt.Errorf("repository is unavailable: %w", err)
	}
	_ = root.Close()
	queue, err := db.ActiveSuggestions(ctx)
	if err != nil {
		return p, err
	}
	p.QueueVersion = database.QueueVersion(queue)
	accepted := map[string]int64{}
	for _, s := range queue {
		if s.Status == "accepted" {
			accepted[s.DocumentPath] = s.ID
		}
	}
	for _, s := range queue {
		a := Assess(ctx, c, db, s, accepted)
		p.Counts[a.State]++
		if a.Retire {
			p.RetireCount++
		}
		p.Items = append(p.Items, a)
	}
	raw, _ := json.Marshal(struct {
		Scope, Version string
		Items          []Assessment
	}{p.WriteScope, p.QueueVersion, p.Items})
	sum := sha256.Sum256(raw)
	p.Token = hex.EncodeToString(sum[:])
	return p, nil
}

// ConfirmReconciliation rechecks files, policy, rename evidence and the queue.
// No file is changed, no proposal accepted, and no model is invoked.
func ConfirmReconciliation(ctx context.Context, c config.Config, db *database.DB, token string) (Reconciliation, error) {
	if job, running, err := db.RunningJob(ctx); err != nil || running {
		if err != nil {
			return Reconciliation{}, err
		}
		return Reconciliation{}, fmt.Errorf("job #%d is running; wait before reconciling", job.ID)
	}
	plan, err := PreviewReconciliation(ctx, c, db)
	if err != nil {
		return plan, err
	}
	if token == "" || token != plan.Token {
		return plan, fmt.Errorf("reconciliation preview changed; preview and confirm again")
	}
	items := []database.Retirement{}
	for _, a := range plan.Items {
		if a.Retire {
			items = append(items, database.Retirement{ID: a.ID, Classification: a.State, Reason: a.Reason, CurrentSHA256: a.CurrentSHA256})
		}
	}
	plan.BatchID, err = db.RetireQueue(ctx, plan.QueueVersion, items)
	if err == nil {
		plan.Retired = len(items)
	}
	return plan, err
}
