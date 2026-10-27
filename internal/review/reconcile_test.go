package review

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
)

func queueFixture(t *testing.T) (config.Config, *database.DB) {
	t.Helper()
	c := config.Config{RepositoryPath: t.TempDir(), DatabasePath: filepath.Join(t.TempDir(), "queue.db")}
	db, err := database.Open(t.Context(), c.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return c, db
}

func queueSuggestion(t *testing.T, c config.Config, db *database.DB, name, original string, s database.Suggestion) database.Suggestion {
	t.Helper()
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, name), []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(frontmatter.Canonical(original)))
	doc := database.Document{Path: name, SHA256: hex.EncodeToString(sum[:]), TagsJSON: "[]"}
	if _, err := db.UpsertScanned(t.Context(), doc, name); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(t.Context(), doc, "fake", "", "", nil, []database.Suggestion{s}); err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := db.QueryRow(`SELECT MAX(id) FROM suggestions`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	got, err := db.Suggestion(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestReconcileReadOnlyPreviewAndAuditedRetirement(t *testing.T) {
	c, db := queueFixture(t)
	valid := queueSuggestion(t, c, db, "valid.md", "# Valid\n", database.Suggestion{
		Action: "expand", ProposedContent: "---\ntitle: Valid\n---\n# Valid\n"})
	invalid := queueSuggestion(t, c, db, "invalid.md", "# Original\n", database.Suggestion{
		Action: "expand", Origin: "deterministic", ProposedContent: "# Replaced\n"})
	stale := queueSuggestion(t, c, db, "stale.md", "# Old\n", database.Suggestion{
		Action: "expand", ProposedContent: "---\ntitle: Old\n---\n# Old\n"})
	missing := queueSuggestion(t, c, db, "missing.md", "# Gone\n", database.Suggestion{Action: "expand", ProposedContent: "# Gone\n"})
	blocked := queueSuggestion(t, c, db, "move.md", "# Move\n", database.Suggestion{Action: "move", TargetPath: "moved.md"})
	body := queueSuggestion(t, c, db, "body.md", "# Body\n", database.Suggestion{Action: "expand", ProposedContent: "# Body\n\nExtra\n"})
	if _, err := db.Exec(`UPDATE suggestions SET source_sha256='older' WHERE id=?`, stale.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(c.RepositoryPath, "missing.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE documents SET status='missing' WHERE path='missing.md'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Decide(t.Context(), blocked.ID, "accepted", ""); err != nil {
		t.Fatal(err)
	}
	before, err := db.ActiveSuggestions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PreviewReconciliation(t.Context(), c, db)
	if err != nil {
		t.Fatal(err)
	}
	if plan.RetireCount != 3 || plan.Counts["valid"] != 1 || plan.Counts["blocked"] != 2 {
		t.Fatalf("unexpected preview: %+v", plan)
	}
	after, _ := db.ActiveSuggestions(t.Context())
	if database.QueueVersion(before) != database.QueueVersion(after) {
		t.Fatal("dry-run changed the queue")
	}
	result, err := ConfirmReconciliation(t.Context(), c, db, plan.Token)
	if err != nil || result.Retired != 3 || result.BatchID == "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, id := range []int64{invalid.ID, stale.ID, missing.ID} {
		s, _ := db.Suggestion(t.Context(), id)
		if s.Status != "superseded" || s.DecisionNote != "" || s.Error == "" {
			t.Fatalf("machine retirement became human feedback: %+v", s)
		}
	}
	for _, id := range []int64{valid.ID, body.ID, blocked.ID} {
		s, _ := db.Suggestion(t.Context(), id)
		if s.Status == "superseded" {
			t.Fatalf("valid/blocked proposal #%d was retired", id)
		}
	}
	var events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM review_events WHERE actor='system' AND batch_id=?`, result.BatchID).Scan(&events); err != nil || events != 3 {
		t.Fatalf("events=%d err=%v", events, err)
	}
	if _, err := ConfirmReconciliation(t.Context(), c, db, plan.Token); err == nil {
		t.Fatal("stale confirmation token was accepted")
	}
}

func TestReconcileRefusesChangedFilesAndQueue(t *testing.T) {
	c, db := queueFixture(t)
	s := queueSuggestion(t, c, db, "a.md", "# A\n", database.Suggestion{Action: "expand", ProposedContent: "---\ntitle: A\n---\n# A\n"})
	plan, err := PreviewReconciliation(t.Context(), c, db)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "a.md"), []byte("# Edited\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ConfirmReconciliation(t.Context(), c, db, plan.Token); err == nil {
		t.Fatal("changed source did not invalidate the preview")
	}
	if err := db.Decide(t.Context(), s.ID, "accepted", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RetireQueue(t.Context(), plan.QueueVersion, []database.Retirement{
		{ID: s.ID, Classification: "stale", Reason: "old preview"},
	}); err == nil {
		t.Fatal("changed database queue did not invalidate the transaction")
	}
}

func TestDefaultScopeBlocksLegacyAcceptedWritesWithoutLosingDecision(t *testing.T) {
	c, db := queueFixture(t)
	s := queueSuggestion(t, c, db, "a.md", "# A\n", database.Suggestion{Action: "move", TargetPath: "b.md"})
	if err := db.Decide(t.Context(), s.ID, "accepted", ""); err != nil {
		t.Fatal(err)
	}
	results, err := Apply(t.Context(), c, db)
	if err != nil || len(results) != 1 || results[0].Status != "blocked" || results[0].Applied {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	current, _ := db.Suggestion(t.Context(), s.ID)
	if current.Status != "accepted" {
		t.Fatalf("a policy block must preserve the review decision: %s", current.Status)
	}
	if _, err := os.Stat(filepath.Join(c.RepositoryPath, "a.md")); err != nil {
		t.Fatal("blocked move changed source")
	}
	if err := DecideCurrent(t.Context(), c, db, "1", "pending", ""); err != nil {
		t.Fatal(err)
	}
	current, _ = db.Suggestion(t.Context(), s.ID)
	if current.Status != "pending" {
		t.Fatal("withdrawal did not restore the proposal")
	}
}

func TestReconcileDoesNotRetireProposalsWhenValidationIsUnavailable(t *testing.T) {
	c, db := queueFixture(t)
	s := queueSuggestion(t, c, db, "a.md", "# A\n", database.Suggestion{
		Action: "expand", ProposedContent: "---\ntitle: A\n---\n# A\n",
	})
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "taxonomy.yaml"), []byte("version: [\n"), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := PreviewReconciliation(t.Context(), c, db)
	if err != nil || plan.RetireCount != 0 || plan.Counts["unavailable"] != 1 {
		t.Fatalf("bad taxonomy retired a valid proposal: %+v %v", plan, err)
	}
	if _, err := ConfirmReconciliation(t.Context(), c, db, plan.Token); err != nil {
		t.Fatal(err)
	}
	got, _ := db.Suggestion(t.Context(), s.ID)
	if got.Status != "pending" {
		t.Fatal("validation outage changed review state")
	}
}
