package review

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/mcppolicy"
	"knowledge-ba46-flow/internal/paths"
	"knowledge-ba46-flow/internal/proposal"
)

type replayRow struct {
	ID       int64  `json:"id"`
	Document string `json:"document"`
	Status   string `json:"original_status"`
	Origin   string `json:"origin"`
	Action   string `json:"action"`
	Outcome  string `json:"preflight"`
	Error    string `json:"error,omitempty"`
	Selected bool   `json:"selected"`
}

type replayReport struct {
	At                string                    `json:"at"`
	SourceSchema      int                       `json:"source_schema"`
	TestSchema        int                       `json:"test_schema"`
	DatabaseStatuses  map[string]int            `json:"database_statuses"`
	OutcomesByStatus  map[string]map[string]int `json:"outcomes_by_status"`
	CopiedFiles       int                       `json:"copied_files"`
	SkippedSymlinks   int                       `json:"skipped_symlinks"`
	RecordedRenames   int                       `json:"recorded_renames"`
	Selected          int                       `json:"selected"`
	Applied           int                       `json:"applied"`
	SourceDBUnchanged bool                      `json:"source_database_unchanged"`
	Completed         bool                      `json:"completed"`
	Rows              []replayRow               `json:"rows"`
	ApplyResults      []ApplyResult             `json:"apply_results"`
	WriteScope        string                    `json:"write_scope"`
	QueueCounts       map[string]int            `json:"queue_counts"`
	Reconciled        int                       `json:"reconciled"`
	ReviewEvents      int                       `json:"review_events"`
}

// Explicit opt-in only: normal tests must not depend on a developer's corpus.
// Pending decisions are simulated exclusively in private temporary copies.
func TestProductionReplay(t *testing.T) {
	configPath := os.Getenv("KBM_REPLAY_CONFIG")
	if configPath == "" {
		t.Skip("set KBM_REPLAY_CONFIG to opt in to a private, offline production replay")
	}
	c, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	report := &replayReport{
		At:               time.Now().UTC().Format(time.RFC3339),
		OutcomesByStatus: map[string]map[string]int{},
	}
	saveReplayReport(t, report)
	sourceFingerprint := replayDatabaseFingerprint(t, c.DatabasePath)
	workspace := t.TempDir()
	clone := c
	clone.RepositoryPath = filepath.Join(workspace, "repository")
	clone.DatabasePath = filepath.Join(workspace, "state.db")
	clone.ReindexTriggerPath = ""
	clone.OllamaURL, clone.OllamaAPIKey = "http://127.0.0.1:9", ""
	if err := snapshotReplayDB(t.Context(), c.DatabasePath, clone.DatabasePath); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", clone.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	err = raw.QueryRow(`PRAGMA user_version`).Scan(&report.SourceSchema)
	raw.Close()
	if err != nil {
		t.Fatal(err)
	}
	// The only migrated database is the newly created snapshot.
	db, err := database.Open(t.Context(), clone.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&report.TestSchema); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM renames`).Scan(&report.RecordedRenames); err != nil {
		t.Fatal(err)
	}
	var running int
	if err := db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status='running'`).Scan(&running); err != nil {
		t.Fatal(err)
	}
	if running != 0 {
		t.Fatal("source has active jobs; wait for writers before taking a repository snapshot")
	}
	report.DatabaseStatuses = replayStatusCounts(t, db)
	files, err := snapshotReplayFiles(c, clone.RepositoryPath)
	if err != nil {
		t.Fatal(err)
	}
	report.CopiedFiles, report.SkippedSymlinks = len(files.Hashes), files.SkippedSymlinks
	verifyReplayFiles(t, c.RepositoryPath, files, nil, max(c.MaxDocumentBytes, 1<<20))
	suggestions := replaySuggestions(t, db)
	root, err := os.OpenRoot(clone.RepositoryPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	selected := map[string]bool{}
	var ids []int64
	for _, s := range suggestions {
		outcome, detail := preflightReplay(t.Context(), clone, db, root, s, files)
		if outcome == "invalid_proposal" && validateForReview(t.Context(), c, db, s) == nil {
			outcome, detail = "snapshot_incomplete", "live read-only validation passes; excluded snapshot files prevent replay"
		}
		row := replayRow{ID: s.ID, Document: s.DocumentPath, Status: s.Status, Origin: s.Origin,
			Action: s.Action, Outcome: outcome, Error: detail}
		if outcome == "valid" && s.Status != "failed" && !selected[s.DocumentPath] {
			row.Selected = true
			selected[s.DocumentPath] = true
			ids = append(ids, s.ID)
		}
		if report.OutcomesByStatus[s.Status] == nil {
			report.OutcomesByStatus[s.Status] = map[string]int{}
		}
		report.OutcomesByStatus[s.Status][outcome]++
		report.Rows = append(report.Rows, row)
	}
	report.Selected = len(ids)
	preview, err := PreviewReconciliation(t.Context(), clone, db)
	if err != nil {
		t.Fatal(err)
	}
	report.WriteScope, report.QueueCounts = preview.WriteScope, preview.Counts
	confirmed, err := ConfirmReconciliation(t.Context(), clone, db, preview.Token)
	if err != nil {
		t.Fatal(err)
	}
	report.Reconciled = confirmed.Retired
	if err := db.QueryRow(`SELECT COUNT(*) FROM review_events WHERE batch_id=? AND actor='system'`, confirmed.BatchID).Scan(&report.ReviewEvents); err != nil {
		t.Fatal(err)
	}
	if report.ReviewEvents != preview.RetireCount || report.Reconciled != preview.RetireCount {
		t.Fatalf("retirement/audit mismatch: preview=%d retired=%d events=%d", preview.RetireCount, report.Reconciled, report.ReviewEvents)
	}
	verifyReplayFiles(t, clone.RepositoryPath, files, nil, max(c.MaxDocumentBytes, 1<<20))
	// Historical accepted moves/deletes must not execute even in the sandbox.
	// Selection is deterministic test input, never a production review decision.
	if _, err := db.Exec(`UPDATE suggestions SET status='pending' WHERE status='accepted'`); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if err := DecideCurrent(t.Context(), clone, db, strconv.FormatInt(id, 10), "accepted", ""); err != nil {
			t.Fatalf("sandbox acceptance #%d disagrees with preflight: %v", id, err)
		}
	}
	report.ApplyResults, err = Apply(t.Context(), clone, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range report.ApplyResults {
		if !result.Applied || result.Action != "expand" || result.Error != "" {
			t.Errorf("unexpected sandbox result #%d: %+v", result.ID, result)
		} else {
			report.Applied++
		}
	}
	if report.Applied != report.Selected {
		t.Errorf("sandbox applied=%d selected=%d", report.Applied, report.Selected)
	}
	verifyReplayFiles(t, clone.RepositoryPath, files, selected, max(c.MaxDocumentBytes, 1<<20))
	verifyReplayFiles(t, c.RepositoryPath, files, nil, max(c.MaxDocumentBytes, 1<<20))
	report.SourceDBUnchanged = sourceFingerprint == replayDatabaseFingerprint(t, c.DatabasePath)
	if !report.SourceDBUnchanged {
		t.Fatal("source database/WAL changed during replay; rerun while production writers are idle")
	}
	report.Completed = !t.Failed()
	t.Logf("source schema=%d test schema=%d copied files=%d source DB/WAL unchanged=%v",
		report.SourceSchema, report.TestSchema, report.CopiedFiles, report.SourceDBUnchanged)
	t.Logf("preflight by original status: %v", report.OutcomesByStatus)
	t.Logf("queue scope=%s classifications=%v retired=%d audit events=%d", report.WriteScope, report.QueueCounts, report.Reconciled, report.ReviewEvents)
	t.Logf("sandbox selected=%d applied=%d (one valid pending/accepted expansion per document; no moves/deletes/merges)",
		report.Selected, report.Applied)
}

func preflightReplay(ctx context.Context, c config.Config, db *database.DB, root *os.Root, s database.Suggestion, files replayFiles) (string, string) {
	if s.Action != "expand" {
		return "excluded_action", "only expansions are permitted in this sandbox"
	}
	if _, err := paths.CleanRelative(s.DocumentPath); err != nil {
		return "invalid_source", err.Error()
	}
	if _, copied := files.Hashes[s.DocumentPath]; !copied {
		return "source_unavailable", "no copied regular source document; missing/hidden/symlink sources are not replayed"
	}
	current, err := hashSource(c.RepositoryPath, s.DocumentPath)
	if err != nil {
		return "source_unavailable", err.Error()
	}
	if current != s.SourceSHA256 {
		return "stale_source", "current canonical content differs from the stored source version"
	}
	if err := mcppolicy.AssertMutationAllowed(root, s.DocumentPath); err != nil {
		return "policy_denied", err.Error()
	}
	if err := validateForReview(ctx, c, db, s); err != nil {
		if errors.Is(err, proposal.ErrWriteScope) || errors.Is(err, ErrReviewBlocked) {
			return "blocked_scope", err.Error()
		}
		if errors.Is(err, ErrValidationUnavailable) {
			return "validation_unavailable", err.Error()
		}
		return "invalid_proposal", err.Error()
	}
	return "valid", ""
}

func replaySuggestions(t *testing.T, db *database.DB) []database.Suggestion {
	t.Helper()
	rows, err := db.Query(`SELECT id,document_path,source_sha256,action,target_path,proposed_content,status,origin
FROM suggestions WHERE status IN ('pending','accepted','failed')
ORDER BY CASE status WHEN 'accepted' THEN 0 WHEN 'pending' THEN 1 ELSE 2 END,id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []database.Suggestion
	for rows.Next() {
		var s database.Suggestion
		if err := rows.Scan(&s.ID, &s.DocumentPath, &s.SourceSHA256, &s.Action, &s.TargetPath,
			&s.ProposedContent, &s.Status, &s.Origin); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func replayStatusCounts(t *testing.T, db *database.DB) map[string]int {
	t.Helper()
	rows, err := db.Query(`SELECT status,COUNT(*) FROM suggestions GROUP BY status`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			t.Fatal(err)
		}
		out[status] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func verifyReplayFiles(t *testing.T, directory string, files replayFiles, allowed map[string]bool, limit int64) {
	t.Helper()
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for path, before := range files.Hashes {
		if allowed[path] {
			continue
		}
		after, err := readReplayFile(root, path, limit)
		if err != nil || before != sha256.Sum256(after) {
			t.Fatalf("snapshot source/unselected file changed (%s); wait for writers and rerun: %v", path, err)
		}
	}
}

func replayDatabaseFingerprint(t *testing.T, path string) string {
	t.Helper()
	var out string
	for _, name := range []string{path, path + "-wal"} {
		f, err := os.Open(name)
		if os.IsNotExist(err) && name != path {
			out += ":no-wal"
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.New()
		_, readErr := io.Copy(sum, f)
		err = errors.Join(readErr, f.Close())
		if err != nil {
			t.Fatal(err)
		}
		out += ":" + hex.EncodeToString(sum.Sum(nil))
	}
	return out
}

func saveReplayReport(t *testing.T, report *replayReport) {
	t.Helper()
	path := os.Getenv("KBM_REPLAY_REPORT")
	if path == "" {
		return
	}
	t.Cleanup(func() {
		content, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Error(err)
			return
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			t.Error(fmt.Errorf("create private replay report (never overwrite): %w", err))
			return
		}
		_, writeErr := f.Write(append(content, '\n'))
		if err := errors.Join(writeErr, f.Close()); err != nil {
			t.Error(err)
		}
	})
}
