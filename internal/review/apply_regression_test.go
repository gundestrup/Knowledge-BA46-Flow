package review

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/scanner"
)

func applyFixture(t *testing.T, original string, suggestions []database.Suggestion) (config.Config, *database.DB, []database.Suggestion) {
	t.Helper()
	c := config.Config{RepositoryPath: t.TempDir(), MaxDocumentBytes: 200000, WriteScope: "all"}
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "note.md"), []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	sum := sha256.Sum256([]byte(frontmatter.Canonical(original)))
	doc := database.Document{Path: "note.md", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(original)), TagsJSON: "[]"}
	if _, err := db.UpsertScanned(t.Context(), doc, "scan"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(t.Context(), doc, "fake", "", "", nil, suggestions); err != nil {
		t.Fatal(err)
	}
	rows, err := db.PendingSuggestions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return c, db, rows
}

func TestApplyRecordsAppliedGraphEdges(t *testing.T) {
	c, db, rows := applyFixture(t, "# Note\n", []database.Suggestion{{
		Action: "expand", ProposedContent: "---\nrelated: [other.md]\n---\n# Note\n",
	}})
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "other.md"), []byte("# Other\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := DecideCurrent(t.Context(), c, db, strconv.FormatInt(rows[0].ID, 10), "accepted", ""); err != nil {
		t.Fatal(err)
	}
	results, err := Apply(t.Context(), c, db)
	if err != nil || len(results) != 1 || !results[0].Applied {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	ok, err := db.AppliedGraphEdgeExists(t.Context(), "note.md", "other.md")
	if err != nil || !ok {
		t.Fatalf("applied edge provenance missing ok=%v err=%v", ok, err)
	}
	ok, err = db.AppliedGraphEdgeExists(t.Context(), "note.md", "unrelated.md")
	if err != nil || ok {
		t.Fatalf("phantom edge provenance ok=%v err=%v", ok, err)
	}
}

func TestApplyPreservesLatestManagedMetadata(t *testing.T) {
	for _, operational := range []string{"", "kbm: {freshness: {verdict: stale}}\n", "kbm: {injected: true}\n"} {
		t.Run(operational, func(t *testing.T) {
			c, db, rows := applyFixture(t, "---\ntags: [go]\n---\n# Note\n", []database.Suggestion{{
				Action: "expand", ProposedContent: "---\ntags: [go, tool]\n" + operational + "---\n# Note\n",
			}})
			root, err := os.OpenRoot(c.RepositoryPath)
			if err != nil {
				t.Fatal(err)
			}
			err = frontmatter.WriteManaged(root, "note.md", "freshness", map[string]any{"verdict": "current"})
			root.Close()
			if err != nil {
				t.Fatal(err)
			}
			if err := DecideCurrent(t.Context(), c, db, strconv.FormatInt(rows[0].ID, 10), "accepted", ""); err != nil {
				t.Fatal(err)
			}
			results, err := Apply(t.Context(), c, db)
			if err != nil || len(results) != 1 || !results[0].Applied {
				t.Fatalf("results=%+v err=%v", results, err)
			}
			raw, _ := os.ReadFile(filepath.Join(c.RepositoryPath, "note.md"))
			if !strings.Contains(string(raw), "current") || strings.Contains(string(raw), "injected") || strings.Contains(string(raw), "stale") {
				t.Fatalf("operational metadata was overwritten: %s", raw)
			}
		})
	}
}

func TestApplyRetiresOtherSnapshotsAndRecordsBatch(t *testing.T) {
	c, db, rows := applyFixture(t, "# Note\n", []database.Suggestion{
		{Action: "expand", ProposedContent: "# Note\n\nFirst\n"},
		{Action: "expand", ProposedContent: "# Note\n\nSecond\n"},
		{Action: "expand", ProposedContent: "# Note\n\nPending\n"},
	})
	for _, s := range rows[:2] {
		if err := db.Decide(t.Context(), s.ID, "accepted", ""); err != nil {
			t.Fatal(err)
		}
	}
	results, err := Apply(t.Context(), c, db)
	if err != nil || len(results) != 2 || !results[0].Applied || results[1].Status != "superseded" {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	for i, s := range rows {
		got, err := db.Suggestion(t.Context(), s.ID)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if got.AppliedAt == "" || got.ApplyBatchID == "" || got.ApplyAttemptedAt == "" {
				t.Fatalf("missing apply history: %+v", got)
			}
		} else if got.Status != "superseded" || !strings.Contains(got.Error, "#"+strconv.FormatInt(rows[0].ID, 10)) {
			t.Fatalf("old proposal not retired with a reason: %+v", got)
		}
	}
	if results[0].BatchID != results[1].BatchID || results[0].DocumentPath != "note.md" {
		t.Fatalf("missing result provenance: %+v", results)
	}
}

func TestDecideCurrentRejectsStaleAndConflictingSnapshots(t *testing.T) {
	c, db, rows := applyFixture(t, "# Note\n", []database.Suggestion{
		{Action: "expand", ProposedContent: "# Note\n\nOne\n"},
		{Action: "expand", ProposedContent: "# Note\n\nTwo\n"},
	})
	decide := func(id int64) error {
		return DecideCurrent(t.Context(), c, db, strconv.FormatInt(id, 10), "accepted", "")
	}
	if err := decide(rows[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := decide(rows[1].ID); err == nil || !strings.Contains(err.Error(), "already accepted") {
		t.Fatalf("expected duplicate-source conflict, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "note.md"), []byte("# Human edit\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := decide(rows[1].ID); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("expected stale source, got %v", err)
	}
	got, _ := db.Suggestion(t.Context(), rows[1].ID)
	if got.Status != "superseded" || got.Error == "" {
		t.Fatalf("stale proposal not retired: %+v", got)
	}
}

func TestInvalidDeterministicBodyCannotBeAccepted(t *testing.T) {
	original := "---\ntags: [go]\n---\n# Note\n\n[Catalog](../catalog.md)\n"
	c, db, rows := applyFixture(t, original, []database.Suggestion{{
		Action: "expand", Origin: "deterministic",
		ProposedContent: strings.Replace(original, "../catalog.md", "catalog.md", 1),
	}})
	err := DecideCurrent(t.Context(), c, db, strconv.FormatInt(rows[0].ID, 10), "accepted", "")
	if err == nil || !strings.Contains(err.Error(), "verified link repair") {
		t.Fatalf("bad local proposal was accepted: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(c.RepositoryPath, "note.md"))
	if string(raw) != original {
		t.Fatal("acceptance changed the repository")
	}
}

func TestApplyingDistinctDocumentsDoesNotInvalidateOtherSources(t *testing.T) {
	c, db, _ := applyFixture(t, "# Note\n", []database.Suggestion{{
		Action: "expand", ProposedContent: "# Note\n\nMore context\n",
	}})
	other := "# Other\n"
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "other.md"), []byte(other), 0644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(other))
	doc := database.Document{Path: "other.md", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(other)), TagsJSON: "[]"}
	if _, err := db.UpsertScanned(t.Context(), doc, "scan"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(t.Context(), doc, "fake", "", "", nil, []database.Suggestion{{
		Action: "expand", ProposedContent: other + "\nMore context\n",
	}}); err != nil {
		t.Fatal(err)
	}
	rows, _ := db.PendingSuggestions(t.Context())
	for _, s := range rows {
		if err := DecideCurrent(t.Context(), c, db, strconv.FormatInt(s.ID, 10), "accepted", ""); err != nil {
			t.Fatal(err)
		}
	}
	results, err := Apply(t.Context(), c, db)
	if err != nil || len(results) != 2 || !results[0].Applied || !results[1].Applied {
		t.Fatalf("distinct sources interfered: %+v, %v", results, err)
	}
}

func TestMergePreservesDestinationManagedMetadata(t *testing.T) {
	repo := t.TempDir()
	for name, body := range map[string]string{"source.md": "Source", "target.md": "Target"} {
		content := "---\nkbm: {owner: " + body + "}\n---\n# " + body + "\n"
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := applyOne(config.Config{RepositoryPath: repo, WriteScope: "all"}, database.Suggestion{
		Action: "merge", DocumentPath: "source.md", TargetPath: "target.md", ProposedContent: "# Source\n\n# Target\n",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(repo, "target.md"))
	fields, _, _ := frontmatter.Parse(string(got))
	if fields["kbm"].(map[string]any)["owner"] != "Target" {
		t.Fatalf("destination operational metadata changed: %s", got)
	}
}

// An accepted frontmatter change is the pipeline's own output — it must not
// re-pend the document or seed dependents on the next scan. The bug: apply
// never restamped, so every accepted fm edit re-queued the doc as if an
// external edit had landed.
func TestApplyRestampsFrontmatterChange(t *testing.T) {
	c, db, rows := applyFixture(t, "# Note\n\nBody here.\n", []database.Suggestion{{
		Action: "expand", Reason: "r", Confidence: 0.9, ProposedContent: "---\ntags: [x]\n---\n# Note\n\nBody here.\n",
	}})
	prompts := filepath.Join(t.TempDir(), "prompts")
	for name, text := range map[string]string{
		"base.md": "B", "knowledge-base-mcp-server.md": "M",
		"analyze.md": "A", "enrich.md": "E", "expand.md": "X", "freshness.md": "F",
	} {
		os.MkdirAll(prompts, 0755)
		os.WriteFile(filepath.Join(prompts, name), []byte(text), 0644)
	}
	c.PromptDirectory = prompts
	for _, s := range rows {
		if err := db.Decide(t.Context(), s.ID, "accepted", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Apply(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	pending, err := db.Pending(t.Context(), "fake", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("fm-only apply must not re-pend the doc, pending=%v", pending)
	}
}

// An applied merge records the absorbed path as a rename so inbound links
// and recorded evidence retarget to the merged note deterministically —
// no LLM pass should be needed to redraw edges.
func TestApplyMergeRecordsRename(t *testing.T) {
	c, db, rows := applyFixture(t, "# Note\n\nMerged away.\n", []database.Suggestion{{
		Action: "merge", TargetPath: "merged.md", Reason: "r", Confidence: 0.9,
		ProposedContent: "# Note\n\nMerged away.\n\n# Merged\n\nOriginal.\n",
	}})
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "merged.md"), []byte("# Merged\n\nOriginal.\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// The merge target must be a live documents row — RetargetEvidence only
	// resolves chains ending at an existing document.
	tgt := database.Document{Path: "merged.md", SHA256: "x", TagsJSON: "[]"}
	if _, err := db.UpsertScanned(t.Context(), tgt, "scan"); err != nil {
		t.Fatal(err)
	}
	for _, s := range rows {
		if err := db.Decide(t.Context(), s.ID, "accepted", ""); err != nil {
			t.Fatal(err)
		}
	}
	results, err := Apply(t.Context(), c, db)
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range results {
		t.Logf("apply %s %s applied=%v err=%q", res.Action, res.DocumentPath, res.Applied, res.Error)
	}
	renames, err := db.Renames(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := renames["note.md"]; got != "merged.md" {
		t.Fatalf("expected rename note.md→merged.md, got %v", renames)
	}
	if target, ok := db.RetargetPath(t.Context(), "note.md"); !ok || target != "merged.md" {
		t.Fatalf("RetargetPath should resolve note.md→merged.md, got %q ok=%v", target, ok)
	}
}

// A body-changing apply is real new content: the doc must stay pending for
// full analysis — only fm-only applies restamp. Pinned by the
// semantic-body equality guard in MarkAppliedCurrent.
func TestApplyBodyChangeStaysPending(t *testing.T) {
	c := config.Config{RepositoryPath: t.TempDir(), MaxDocumentBytes: 200000, WriteScope: "all", Extensions: []string{".md"}}
	prompts := filepath.Join(t.TempDir(), "prompts")
	for name, text := range map[string]string{
		"base.md": "B", "knowledge-base-mcp-server.md": "M",
		"analyze.md": "A", "enrich.md": "E", "expand.md": "X", "freshness.md": "F",
	} {
		os.MkdirAll(prompts, 0755)
		os.WriteFile(filepath.Join(prompts, name), []byte(text), 0644)
	}
	c.PromptDirectory = prompts
	body := "Body here.\n\nLine two.\n\nLine three.\n\nLine four.\n\nLine five.\n\nLine six.\n"
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "note.md"), []byte("# Note\n\n"+body), 0644); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	docs, err := db.Documents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var doc database.Document
	for _, d := range docs {
		if d.Path == "note.md" {
			doc = d
		}
	}
	sug := database.Suggestion{
		Action: "expand", Reason: "r", Confidence: 0.9,
		ProposedContent: "# Note\n\n" + body + "\nNew paragraph of substance.\n",
	}
	if err := db.SaveAnalysis(t.Context(), doc, "fake", "", "", nil, []database.Suggestion{sug}); err != nil {
		t.Fatal(err)
	}
	rows, err := db.PendingSuggestions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range rows {
		if err := db.Decide(t.Context(), s.ID, "accepted", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Apply(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	pending, err := db.Pending(t.Context(), "fake", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Path != "note.md" {
		t.Fatalf("body-changing apply must leave the doc pending, got %v", pending)
	}
}
