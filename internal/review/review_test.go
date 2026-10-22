package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/paths"
)

func seedReviewSuggestions(t *testing.T) (*database.DB, []database.Suggestion) {
	t.Helper()
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	doc := database.Document{Path: "guide.md", SHA256: "hash", Size: 1, TagsJSON: "[]"}
	if _, err := db.UpsertScanned(context.Background(), doc, "scan"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(context.Background(), doc, "test-model", "Guide", "Summary", nil, []database.Suggestion{
		{Action: "move", TargetPath: "new/guide.md", Reason: "better location", Confidence: .9},
		{Action: "expand", Reason: "add examples", Confidence: .7, ProposedContent: "# Guide\n\nExamples"},
		{Action: "delete", Reason: "duplicate", Confidence: .5},
	}); err != nil {
		t.Fatal(err)
	}
	suggestions, err := db.PendingSuggestions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return db, suggestions
}

func captureStdout(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	callErr := fn()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = old
	output, readErr := io.ReadAll(reader)
	reader.Close()
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(output), callErr
}

func TestApplyExpandMoveAndDelete(t *testing.T) {
	repo := t.TempDir()
	c := config.Config{RepositoryPath: repo, TrashDirectory: ".kbm-trash"}
	p := filepath.Join(repo, "a.md")
	os.WriteFile(p, []byte("old"), 0644)
	if err := applyOne(c, database.Suggestion{Action: "expand", DocumentPath: "a.md", ProposedContent: "old new"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if string(b) != "old new" {
		t.Fatalf("got %q", b)
	}
	if err := applyOne(c, database.Suggestion{Action: "move", DocumentPath: "a.md", TargetPath: "folder/b.md"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, "folder", "b.md")); err != nil {
		t.Fatal(err)
	}
	if err := applyOne(c, database.Suggestion{Action: "delete", DocumentPath: "folder/b.md"}); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(repo, ".kbm-trash", "*", "folder", "b.md"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("trash=%v err=%v", matches, err)
	}
}

func TestSafePathRejectsTraversal(t *testing.T) {
	if _, err := safePath(t.TempDir(), "../escape.md"); err == nil {
		t.Fatal("expected traversal rejection")
	}
	if _, err := safePath(t.TempDir(), ""); err == nil {
		t.Fatal("expected empty path rejection")
	}
	if _, err := safePath(t.TempDir(), "."); err == nil {
		t.Fatal("expected repository root rejection")
	}
	if _, err := safePath(t.TempDir(), filepath.Join(string(os.PathSeparator), "outside.md")); err == nil {
		t.Fatal("expected absolute path rejection")
	}
}

func TestMergeWritesTargetAndTrashesSource(t *testing.T) {
	repo := t.TempDir()
	c := config.Config{RepositoryPath: repo, TrashDirectory: ".kbm-trash"}
	os.WriteFile(filepath.Join(repo, "source.md"), []byte("source"), 0644)
	os.WriteFile(filepath.Join(repo, "target.md"), []byte("target"), 0644)
	err := applyOne(c, database.Suggestion{Action: "merge", DocumentPath: "source.md", TargetPath: "target.md", ProposedContent: "source target combined"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(repo, "target.md"))
	if strings.TrimSpace(string(b)) != "source target combined" {
		t.Fatalf("got %q", b)
	}
}

func TestApplyHonorsMCPWritePolicy(t *testing.T) {
	t.Run("deny blocks expand and leaves source unchanged", func(t *testing.T) {
		repo := t.TempDir()
		source := filepath.Join(repo, "note.md")
		if err := os.WriteFile(source, []byte("# original\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, ".kb-policy.json"), []byte(`{"mutations":"deny"}`), 0600); err != nil {
			t.Fatal(err)
		}
		err := applyOne(config.Config{RepositoryPath: repo}, database.Suggestion{
			Action: "expand", DocumentPath: "note.md", ProposedContent: "# replacement\n",
		})
		if err == nil || !strings.Contains(err.Error(), "denies mutations") {
			t.Fatalf("error=%v, want policy denial", err)
		}
		got, readErr := os.ReadFile(source)
		if readErr != nil || string(got) != "# original\n" {
			t.Fatalf("source=%q readErr=%v", got, readErr)
		}
	})

	t.Run("destination shelf deny blocks cross-shelf move", func(t *testing.T) {
		repo := t.TempDir()
		for _, shelf := range []string{"alpha", "beta"} {
			if err := os.MkdirAll(filepath.Join(repo, shelf), 0755); err != nil {
				t.Fatal(err)
			}
		}
		source := filepath.Join(repo, "alpha", "note.md")
		if err := os.WriteFile(source, []byte("# note\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, "beta", ".kb-policy.json"), []byte(`{"mutations":"deny"}`), 0600); err != nil {
			t.Fatal(err)
		}
		err := applyOne(config.Config{RepositoryPath: repo}, database.Suggestion{
			Action: "move", DocumentPath: "alpha/note.md", TargetPath: "beta/note.md",
		})
		if err == nil || !strings.Contains(err.Error(), "beta/.kb-policy.json") {
			t.Fatalf("error=%v, want destination-shelf denial", err)
		}
		if _, statErr := os.Stat(source); statErr != nil {
			t.Fatalf("source was moved despite denied destination: %v", statErr)
		}
		if _, statErr := os.Stat(filepath.Join(repo, "beta", "note.md")); !os.IsNotExist(statErr) {
			t.Fatalf("destination exists after denied move: err=%v", statErr)
		}
	})

	t.Run("deny blocks delete", func(t *testing.T) {
		repo := t.TempDir()
		source := filepath.Join(repo, "note.md")
		if err := os.WriteFile(source, []byte("# original\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, ".kb-policy.json"), []byte(`{"mutations":"deny"}`), 0600); err != nil {
			t.Fatal(err)
		}
		err := applyOne(config.Config{RepositoryPath: repo}, database.Suggestion{
			Action: "delete", DocumentPath: "note.md",
		})
		if err == nil || !strings.Contains(err.Error(), "denies mutations") {
			t.Fatalf("error=%v, want policy denial", err)
		}
		if _, statErr := os.Stat(source); statErr != nil {
			t.Fatalf("source was moved despite denied delete: %v", statErr)
		}
	})

	t.Run("destination shelf deny blocks merge", func(t *testing.T) {
		repo := t.TempDir()
		targetDir := filepath.Join(repo, "beta")
		if err := os.MkdirAll(targetDir, 0755); err != nil {
			t.Fatal(err)
		}
		source := filepath.Join(repo, "source.md")
		target := filepath.Join(targetDir, "target.md")
		if err := os.WriteFile(source, []byte("# source\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("# target\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(targetDir, ".kb-policy.json"), []byte(`{"mutations":"deny"}`), 0600); err != nil {
			t.Fatal(err)
		}
		err := applyOne(config.Config{RepositoryPath: repo}, database.Suggestion{
			Action: "merge", DocumentPath: "source.md", TargetPath: "beta/target.md",
			ProposedContent: "# combined\n",
		})
		if err == nil || !strings.Contains(err.Error(), "beta/.kb-policy.json") {
			t.Fatalf("error=%v, want destination-shelf denial", err)
		}
		for path, want := range map[string]string{source: "# source\n", target: "# target\n"} {
			got, readErr := os.ReadFile(path)
			if readErr != nil || string(got) != want {
				t.Fatalf("path=%s content=%q readErr=%v", path, got, readErr)
			}
		}
	})

	t.Run("policy file cannot itself be changed", func(t *testing.T) {
		repo := t.TempDir()
		policyPath := filepath.Join(repo, ".kb-policy.json")
		original := `{"mutations":"allow"}`
		if err := os.WriteFile(policyPath, []byte(original), 0600); err != nil {
			t.Fatal(err)
		}
		err := applyOne(config.Config{RepositoryPath: repo}, database.Suggestion{
			Action: "expand", DocumentPath: ".kb-policy.json", ProposedContent: `{"mutations":"deny"}`,
		})
		if err == nil || !strings.Contains(err.Error(), "managed policy") {
			t.Fatalf("error=%v, want protected-policy rejection", err)
		}
		got, readErr := os.ReadFile(policyPath)
		if readErr != nil || string(got) != original {
			t.Fatalf("policy=%q readErr=%v", got, readErr)
		}
	})
}

func TestApplyRejectsExistingMoveTarget(t *testing.T) {
	repo := t.TempDir()
	c := config.Config{RepositoryPath: repo, TrashDirectory: ".kbm-trash"}
	source := filepath.Join(repo, "source.md")
	target := filepath.Join(repo, "target.md")
	if err := os.WriteFile(source, []byte("source"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}

	err := applyOne(c, database.Suggestion{Action: "move", DocumentPath: "source.md", TargetPath: "target.md"})
	if err == nil {
		t.Fatal("expected move collision error")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "keep" {
		t.Fatalf("target overwritten: %q", got)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatalf("source should remain: %v", err)
	}
}

func TestApplyRejectsInvalidSuggestions(t *testing.T) {
	repo := t.TempDir()
	c := config.Config{RepositoryPath: repo, TrashDirectory: ".kbm-trash"}
	if err := os.WriteFile(filepath.Join(repo, "source.md"), []byte("source"), 0644); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name           string
		suggestion     database.Suggestion
		trashDirectory string
	}{
		{name: "expand without content", suggestion: database.Suggestion{Action: "expand", DocumentPath: "source.md"}},
		{name: "merge without target", suggestion: database.Suggestion{Action: "merge", DocumentPath: "source.md", ProposedContent: "combined"}},
		{name: "unsupported action", suggestion: database.Suggestion{Action: "teleport", DocumentPath: "source.md"}},
		{name: "trash path escapes", suggestion: database.Suggestion{Action: "delete", DocumentPath: "source.md"}, trashDirectory: "../outside"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testConfig := c
			if tt.trashDirectory != "" {
				testConfig.TrashDirectory = tt.trashDirectory
			}
			if err := applyOne(testConfig, tt.suggestion); err == nil {
				t.Fatal("expected invalid suggestion to fail")
			}
		})
	}
}

func TestApplyCannotEscapeRepositoryThroughSymlinkedParent(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "external.md"), []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "outside")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "source.md"), []byte("source"), 0644); err != nil {
		t.Fatal(err)
	}
	c := config.Config{RepositoryPath: repo, TrashDirectory: ".kbm-trash"}

	err := applyOne(c, database.Suggestion{
		Action: "merge", DocumentPath: "source.md", TargetPath: "outside/external.md", ProposedContent: "overwritten",
	})
	if err == nil {
		t.Fatal("expected symlink escape to be rejected")
	}
	content, err := os.ReadFile(filepath.Join(outside, "external.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "keep" {
		t.Fatalf("external file changed: %q", content)
	}
}

func TestExpandPreservesFilePermissions(t *testing.T) {
	repo := t.TempDir()
	path := filepath.Join(repo, "private.md")
	if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	c := config.Config{RepositoryPath: repo, TrashDirectory: ".kbm-trash"}
	if err := applyOne(c, database.Suggestion{Action: "expand", DocumentPath: "private.md", ProposedContent: "old new"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("permissions=%#o want %#o", got, 0600)
	}
}

func TestExpandRejectsNonRegularTargets(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, "folder"), 0755); err != nil {
		t.Fatal(err)
	}
	c := config.Config{RepositoryPath: repo, TrashDirectory: ".kbm-trash"}
	err := applyOne(c, database.Suggestion{
		Action: "expand", DocumentPath: "folder", ProposedContent: "not a file",
	})
	if err == nil {
		t.Fatal("expected directory target to be rejected")
	}
}

func TestApplyMarksFailuresAndContinues(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	c := config.Config{RepositoryPath: repo, TrashDirectory: ".kbm-trash"}
	fileContents := map[string]string{
		"source.md": "source",
		"target.md": "keep",
		"expand.md": "old",
	}
	for path, content := range fileContents {
		if err := os.WriteFile(filepath.Join(repo, path), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	suggestions := []database.Suggestion{
		{Action: "move", TargetPath: "target.md", Reason: "collision"},
		{Action: "expand", ProposedContent: "old\n\n# Expanded"},
	}
	for i, path := range []string{"source.md", "expand.md"} {
		sum := sha256.Sum256([]byte(fileContents[path]))
		doc := database.Document{Path: path, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(fileContents[path])), TagsJSON: "[]"}
		if _, err := db.UpsertScanned(ctx, doc, "scan"); err != nil {
			t.Fatal(err)
		}
		if err := db.SaveAnalysis(ctx, doc, "test-model", path, "", nil, []database.Suggestion{suggestions[i]}); err != nil {
			t.Fatal(err)
		}
		pending, err := db.PendingSuggestions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, suggestion := range pending {
			if suggestion.DocumentPath == path {
				if err := db.Decide(ctx, suggestion.ID, "accepted", ""); err != nil {
					t.Fatal(err)
				}
				break
			}
		}
	}

	results, err := Apply(ctx, c, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].Applied || results[0].Status != "failed" ||
		results[0].Error == "" || !results[1].Applied || results[1].Status != "applied" {
		t.Fatalf("apply results=%+v", results)
	}
	first, err := db.Suggestion(ctx, results[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.Suggestion(ctx, results[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "failed" || second.Status != "applied" {
		t.Fatalf("statuses first=%q second=%q", first.Status, second.Status)
	}
}

func TestApplySupersedesSuggestionIfSourceChangedAfterReview(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	path := filepath.Join(repo, "guide.md")
	original := []byte("# Original")
	if err := os.WriteFile(path, original, 0644); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sum := sha256.Sum256(original)
	doc := database.Document{Path: "guide.md", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(original)), TagsJSON: "[]"}
	if _, err := db.UpsertScanned(ctx, doc, "scan"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(ctx, doc, "test-model", "Guide", "", nil, []database.Suggestion{
		{Action: "expand", Reason: "add details", ProposedContent: "# Updated by LLM"},
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingSuggestions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending=%+v", pending)
	}
	if err := db.Decide(ctx, pending[0].ID, "accepted", ""); err != nil {
		t.Fatal(err)
	}

	changed := []byte("# Edited after analysis")
	if err := os.WriteFile(path, changed, 0644); err != nil {
		t.Fatal(err)
	}
	results, err := Apply(ctx, config.Config{RepositoryPath: repo, TrashDirectory: ".kbm-trash"}, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Applied || results[0].Status != "superseded" ||
		!strings.Contains(results[0].Error, "changed after analysis") {
		t.Fatalf("results=%+v", results)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(changed) {
		t.Fatalf("stale suggestion modified source: %q", got)
	}
	suggestion, err := db.Suggestion(ctx, pending[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if suggestion.Status != "superseded" {
		t.Fatalf("status=%q", suggestion.Status)
	}
}

func TestListPrintsPendingSuggestionDetails(t *testing.T) {
	db, _ := seedReviewSuggestions(t)
	output, err := captureStdout(t, func() error {
		return List(context.Background(), db)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"#1", "move", "guide.md", "new/guide.md", "better location"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("output missing %q:\n%s", expected, output)
		}
	}
}

func TestListReportsNoSuggestions(t *testing.T) {
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	output, err := captureStdout(t, func() error {
		return List(context.Background(), db)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "No pending suggestions") {
		t.Fatalf("output=%q", output)
	}
}

func TestInteractiveAcceptRejectAndSkip(t *testing.T) {
	db, suggestions := seedReviewSuggestions(t)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	for _, answer := range []string{"y", "n", "s"} {
		if _, err := fmt.Fprintln(writer, answer); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	defer func() { os.Stdin = oldStdin }()
	os.Stdin = reader
	_, callErr := captureStdout(t, func() error {
		return Interactive(context.Background(), db)
	})
	os.Stdin = oldStdin
	reader.Close()
	if callErr != nil {
		t.Fatal(callErr)
	}

	statuses := make(map[string]string)
	for _, suggestion := range suggestions {
		got, err := db.Suggestion(context.Background(), suggestion.ID)
		if err != nil {
			t.Fatal(err)
		}
		statuses[got.Action] = got.Status
	}
	if statuses["move"] != "accepted" || statuses["expand"] != "rejected" || statuses["delete"] != "pending" {
		t.Fatalf("statuses=%v", statuses)
	}
}

func TestInteractiveQuitLeavesRemainingSuggestionsPending(t *testing.T) {
	db, suggestions := seedReviewSuggestions(t)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(writer, "q"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	defer func() { os.Stdin = oldStdin }()
	os.Stdin = reader
	_, callErr := captureStdout(t, func() error {
		return Interactive(context.Background(), db)
	})
	os.Stdin = oldStdin
	reader.Close()
	if callErr != nil {
		t.Fatal(callErr)
	}
	for _, suggestion := range suggestions {
		got, err := db.Suggestion(context.Background(), suggestion.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "pending" {
			t.Fatalf("quit changed suggestion #%d to %q", got.ID, got.Status)
		}
	}
}

func TestDecideRejectsNonNumericID(t *testing.T) {
	db, _ := seedReviewSuggestions(t)
	if err := Decide(context.Background(), db, "not-an-id", "accepted", ""); err == nil {
		t.Fatal("expected invalid ID error")
	}
}

func TestDecideAcceptsNumericID(t *testing.T) {
	db, suggestions := seedReviewSuggestions(t)
	if err := Decide(context.Background(), db, fmt.Sprintf("%d", suggestions[0].ID), "accepted", ""); err != nil {
		t.Fatal(err)
	}
	got, err := db.Suggestion(context.Background(), suggestions[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "accepted" {
		t.Fatalf("status=%q", got.Status)
	}
}

// A vanished source is stale context: the suggestion is superseded, not
// failed — the same treatment as a changed hash, and consistent with a scan
// having marked the document missing.
func TestApplySupersedesWhenSourceFileDisappeared(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	path := filepath.Join(repo, "gone.md")
	content := []byte("# Gone")
	if err := os.WriteFile(path, content, 0644); err != nil {
		t.Fatal(err)
	}
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sum := sha256.Sum256(content)
	doc := database.Document{Path: "gone.md", SHA256: hex.EncodeToString(sum[:]), Size: int64(len(content)), TagsJSON: "[]"}
	if _, err := db.UpsertScanned(ctx, doc, "scan"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(ctx, doc, "test-model", "Gone", "", nil, []database.Suggestion{
		{Action: "expand", ProposedContent: "# New"},
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingSuggestions(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	if err := db.Decide(ctx, pending[0].ID, "accepted", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	results, err := Apply(ctx, config.Config{RepositoryPath: repo, TrashDirectory: ".kbm-trash"}, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Applied || results[0].Status != "superseded" || results[0].Error == "" {
		t.Fatalf("results=%+v", results)
	}
	got, err := db.Suggestion(ctx, pending[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "superseded" {
		t.Fatalf("status=%q", got.Status)
	}
}

func TestApplyPropagatesDatabaseErrors(t *testing.T) {
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Apply(context.Background(), config.Config{RepositoryPath: t.TempDir()}, db); err == nil {
		t.Fatal("expected an error applying against a closed database")
	}
}

func TestMoveAndMergeRejectIdenticalSourceAndTarget(t *testing.T) {
	repo := t.TempDir()
	c := config.Config{RepositoryPath: repo, TrashDirectory: ".kbm-trash"}
	if err := os.WriteFile(filepath.Join(repo, "a.md"), []byte("a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := applyOne(c, database.Suggestion{Action: "move", DocumentPath: "a.md", TargetPath: "a.md"}); err == nil {
		t.Fatal("expected a self-move to be rejected")
	}
	if err := applyOne(c, database.Suggestion{Action: "merge", DocumentPath: "a.md", TargetPath: "a.md", ProposedContent: "x"}); err == nil {
		t.Fatal("expected a self-merge to be rejected")
	}
}

func TestMergeRequiresExistingTarget(t *testing.T) {
	repo := t.TempDir()
	c := config.Config{RepositoryPath: repo, TrashDirectory: ".kbm-trash"}
	if err := os.WriteFile(filepath.Join(repo, "source.md"), []byte("source"), 0644); err != nil {
		t.Fatal(err)
	}
	err := applyOne(c, database.Suggestion{
		Action: "merge", DocumentPath: "source.md", TargetPath: "missing.md", ProposedContent: "combined",
	})
	if err == nil {
		t.Fatal("expected a merge into a missing target to fail")
	}
	if _, err := os.Stat(filepath.Join(repo, "source.md")); err != nil {
		t.Fatalf("source must survive a failed merge: %v", err)
	}
}

func TestExpandRejectsMissingAndSymlinkedFiles(t *testing.T) {
	repo := t.TempDir()
	c := config.Config{RepositoryPath: repo, TrashDirectory: ".kbm-trash"}
	if err := applyOne(c, database.Suggestion{Action: "expand", DocumentPath: "missing.md", ProposedContent: "x"}); err == nil {
		t.Fatal("expected expanding a missing file to fail")
	}
	if err := os.WriteFile(filepath.Join(repo, "real.md"), []byte("real"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.md", filepath.Join(repo, "link.md")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if err := applyOne(c, database.Suggestion{Action: "expand", DocumentPath: "link.md", ProposedContent: "x"}); err == nil {
		t.Fatal("expected expanding a symlink to be rejected")
	}
	content, err := os.ReadFile(filepath.Join(repo, "real.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "real" {
		t.Fatalf("symlink target was modified: %q", content)
	}
}

func TestDeleteUsesDefaultTrashDirectory(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "old.md"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	// Empty trash_directory falls back to .kbm-trash inside applyOne.
	c := config.Config{RepositoryPath: repo}
	if err := applyOne(c, database.Suggestion{Action: "delete", DocumentPath: "old.md"}); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(repo, ".kbm-trash", "*", "old.md"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("trash=%v err=%v", matches, err)
	}
}

func TestMoveCannotEscapeThroughSymlinkedParent(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(repo, "outside")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "a.md"), []byte("a"), 0644); err != nil {
		t.Fatal(err)
	}
	c := config.Config{RepositoryPath: repo, TrashDirectory: ".kbm-trash"}
	err := applyOne(c, database.Suggestion{Action: "move", DocumentPath: "a.md", TargetPath: "outside/evil.md"})
	if err == nil {
		t.Fatal("expected a move through a symlinked parent to be rejected")
	}
	if _, err := os.Stat(filepath.Join(outside, "evil.md")); !os.IsNotExist(err) {
		t.Fatalf("file escaped the repository: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "a.md")); err != nil {
		t.Fatalf("source must survive a failed move: %v", err)
	}
}

func TestDeleteCannotReachThroughSymlinkedParent(t *testing.T) {
	repo := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(secret, []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "outside")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	c := config.Config{RepositoryPath: repo, TrashDirectory: ".kbm-trash"}
	err := applyOne(c, database.Suggestion{Action: "delete", DocumentPath: "outside/secret.md"})
	if err == nil {
		t.Fatal("expected a delete through a symlinked parent to be rejected")
	}
	content, err := os.ReadFile(secret)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "secret" {
		t.Fatalf("external file changed: %q", content)
	}
}

func TestCleanRelativeRejectsAbsolutePaths(t *testing.T) {
	if _, err := paths.CleanRelative(filepath.Join(string(os.PathSeparator), "tmp", "abs.md")); err == nil {
		t.Fatal("expected an absolute path to be rejected")
	}
	got, err := paths.CleanRelative("nested/doc.md")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join("nested", "doc.md") {
		t.Fatalf("cleaned=%q", got)
	}
}

func TestExpandLeavesNoTemporaryFiles(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "a.md"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	c := config.Config{RepositoryPath: repo, TrashDirectory: ".kbm-trash"}
	if err := applyOne(c, database.Suggestion{Action: "expand", DocumentPath: "a.md", ProposedContent: "old new"}); err != nil {
		t.Fatal(err)
	}
	leftover, err := filepath.Glob(filepath.Join(repo, ".kbm-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(leftover) != 0 {
		t.Fatalf("temporary files left behind: %v", leftover)
	}
}

func TestApplyOneIsSafeForConcurrentCalls(t *testing.T) {
	repo := t.TempDir()
	c := config.Config{RepositoryPath: repo, TrashDirectory: ".kbm-trash"}
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("doc%d.md", i)
		if err := os.WriteFile(filepath.Join(repo, name), []byte("old"), 0644); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			errs <- applyOne(c, database.Suggestion{Action: "expand", DocumentPath: name, ProposedContent: "old new " + name})
		}(name)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestListAndInteractivePropagateDatabaseErrors(t *testing.T) {
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := List(context.Background(), db); err == nil {
		t.Fatal("expected List to fail on a closed database")
	}
	if err := Interactive(context.Background(), db); err == nil {
		t.Fatal("expected Interactive to fail on a closed database")
	}
}

func TestInteractiveEOFLeavesAllPending(t *testing.T) {
	db, suggestions := seedReviewSuggestions(t)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	defer func() { os.Stdin = oldStdin }()
	os.Stdin = reader
	_, callErr := captureStdout(t, func() error {
		return Interactive(context.Background(), db)
	})
	os.Stdin = oldStdin
	reader.Close()
	if callErr != nil {
		t.Fatal(callErr)
	}
	for _, suggestion := range suggestions {
		got, err := db.Suggestion(context.Background(), suggestion.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "pending" {
			t.Fatalf("EOF changed suggestion #%d to %q", got.ID, got.Status)
		}
	}
}

func TestApplyRejectsMalformedProposedFrontmatter(t *testing.T) {
	repo := t.TempDir()
	path := filepath.Join(repo, "doc.md")
	os.WriteFile(path, []byte("# Doc\n"), 0644)
	sum := sha256.Sum256([]byte("# Doc\n"))
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	doc := database.Document{Path: "doc.md", SHA256: hex.EncodeToString(sum[:]), Size: 6, TagsJSON: "[]"}
	if _, err := db.UpsertScanned(ctx, doc, "s"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(ctx, doc, "test-model", "t", "s", nil, []database.Suggestion{
		{Action: "expand", Reason: "bad fm", ProposedContent: "---\ntitle: [\n---\n# Doc\n"},
	}); err != nil {
		t.Fatal(err)
	}
	sugs, _ := db.PendingSuggestions(ctx)
	if err := db.Decide(ctx, sugs[0].ID, "accepted", ""); err != nil {
		t.Fatal(err)
	}
	c := config.Config{RepositoryPath: repo, TrashDirectory: ".kbm-trash"}
	results, err := Apply(ctx, c, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "failed" || !strings.Contains(results[0].Error, "frontmatter") {
		t.Fatalf("results=%+v", results)
	}
	// The file is untouched.
	b, _ := os.ReadFile(path)
	if string(b) != "# Doc\n" {
		t.Fatalf("file modified: %q", b)
	}
}

func TestPurgeTrashRemovesOnlyOldEntries(t *testing.T) {
	repo := t.TempDir()
	trash := filepath.Join(repo, ".kbm-trash")
	old := filepath.Join(trash, "20200101T000000.000000000Z", "gone.md")
	fresh := filepath.Join(trash, "29990101T000000.000000000Z", "gone.md")
	for _, p := range []string{old, fresh} {
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("old content"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Now().AddDate(0, 0, -40)
	if err := os.Chtimes(filepath.Dir(old), oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	c := config.Config{RepositoryPath: repo, TrashDirectory: ".kbm-trash"}
	removed, err := PurgeTrash(c, time.Now().AddDate(0, 0, -30))
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	if _, err := os.Stat(filepath.Dir(old)); !os.IsNotExist(err) {
		t.Fatalf("old trash entry should be gone: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(fresh)); err != nil {
		t.Fatalf("fresh trash entry removed: %v", err)
	}
	// A missing trash directory is not an error.
	c.RepositoryPath = t.TempDir()
	if removed, err := PurgeTrash(c, time.Now()); err != nil || removed != 0 {
		t.Fatalf("empty repo: removed=%d err=%v", removed, err)
	}
}

func TestApplyTouchesReindexTriggerAfterSuccess(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, "a.md"), []byte("old"), 0644)
	sum := sha256.Sum256([]byte("old"))
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	doc := database.Document{Path: "a.md", SHA256: hex.EncodeToString(sum[:]), Size: 3, TagsJSON: "[]"}
	if _, err := db.UpsertScanned(ctx, doc, "scan"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(ctx, doc, "test-model", "t", "", nil, []database.Suggestion{
		{Action: "expand", ProposedContent: "old new"},
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingSuggestions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range pending {
		if err := db.Decide(ctx, s.ID, "accepted", ""); err != nil {
			t.Fatal(err)
		}
	}

	c := config.Config{RepositoryPath: repo, TrashDirectory: ".kbm-trash", ReindexTriggerPath: ".reindex-trigger"}
	results, err := Apply(ctx, c, db)
	if err != nil {
		t.Fatal(err)
	}
	var trigger *ApplyResult
	for i := range results {
		if results[i].Action == "reindex-trigger" {
			trigger = &results[i]
		}
	}
	if trigger == nil || !trigger.Applied {
		t.Fatalf("expected an applied reindex-trigger row, got %+v", results)
	}
	st, err := os.Stat(filepath.Join(repo, ".reindex-trigger"))
	if err != nil || st.Size() != 0 {
		t.Fatalf("trigger file: %v %v", st, err)
	}
}
