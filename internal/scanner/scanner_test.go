package scanner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/paths"
)

func TestScanDetectsOnlyChangedMarkdown(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	os.MkdirAll(filepath.Join(repo, "frameworks", "bulma"), 0755)
	md := filepath.Join(repo, "frameworks", "bulma", "theme_controller.md")
	os.WriteFile(md, []byte("# one"), 0644)
	os.WriteFile(filepath.Join(repo, "ignore.txt"), []byte("x"), 0644)
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := config.Config{RepositoryPath: repo, Extensions: []string{".md"}, ExcludeDirectories: []string{".git"}, TrashDirectory: ".kbm-trash", MaxDocumentBytes: 10000}
	first, err := Scan(context.Background(), c, db)
	if err != nil {
		t.Fatal(err)
	}
	if first.Seen != 1 || first.Changed != 1 {
		t.Fatalf("first=%+v", first)
	}
	second, err := Scan(context.Background(), c, db)
	if err != nil {
		t.Fatal(err)
	}
	if second.Changed != 0 {
		t.Fatalf("second=%+v", second)
	}
	os.WriteFile(md, []byte("# two"), 0644)
	third, err := Scan(context.Background(), c, db)
	if err != nil {
		t.Fatal(err)
	}
	if third.Changed != 1 {
		t.Fatalf("third=%+v", third)
	}
	docs, err := db.Pending(context.Background(), "test-model", "test-model", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Path != "frameworks/bulma/theme_controller.md" {
		t.Fatalf("docs=%+v", docs)
	}
}

func TestPathTags(t *testing.T) {
	got := pathTags("frameworks/bulma/theme_controller.md")
	want := []string{"frameworks", "bulma", "theme-controller"}
	if len(got) != len(want) {
		t.Fatalf("got=%v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got=%v want=%v", got, want)
		}
	}
}

func TestScanExcludesDirectoriesAndMarksRemovedFilesMissing(t *testing.T) {
	repo := t.TempDir()
	excluded := filepath.Join(repo, "vendor")
	if err := os.MkdirAll(excluded, 0755); err != nil {
		t.Fatal(err)
	}
	visible := filepath.Join(repo, "visible.md")
	if err := os.WriteFile(visible, []byte("# visible"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(excluded, "ignored.md"), []byte("# ignored"), 0644); err != nil {
		t.Fatal(err)
	}

	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := config.Config{
		RepositoryPath:     repo,
		Extensions:         []string{".md"},
		ExcludeDirectories: []string{"vendor"},
		TrashDirectory:     ".kbm-trash",
	}

	first, err := Scan(context.Background(), c, db)
	if err != nil {
		t.Fatal(err)
	}
	if first.Seen != 1 || first.Changed != 1 {
		t.Fatalf("first scan=%+v", first)
	}
	if err := os.Remove(visible); err != nil {
		t.Fatal(err)
	}
	second, err := Scan(context.Background(), c, db)
	if err != nil {
		t.Fatal(err)
	}
	if second.Seen != 0 {
		t.Fatalf("second scan=%+v", second)
	}
	docs, err := db.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Path != "visible.md" || docs[0].Status != "missing" {
		t.Fatalf("documents=%+v", docs)
	}
}

func TestScanAndReadDocumentDoNotFollowExternalSymlinks(t *testing.T) {
	repo := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("# secret"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(repo, "linked.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}

	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := config.Config{RepositoryPath: repo, Extensions: []string{".md"}, MaxDocumentBytes: 1000}
	result, err := Scan(context.Background(), c, db)
	if err != nil {
		t.Fatal(err)
	}
	if result.Seen != 0 {
		t.Fatalf("symlink was scanned: %+v", result)
	}
	if _, err := ReadDocument(c, "linked.md"); err == nil {
		t.Fatal("expected an external symlink read to be rejected")
	}
}

func TestReadDocumentEnforcesSizeLimit(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "large.md"), []byte("12345"), 0644); err != nil {
		t.Fatal(err)
	}
	c := config.Config{RepositoryPath: repo, MaxDocumentBytes: 4}
	if _, err := ReadDocument(c, "large.md"); err == nil {
		t.Fatal("expected size-limit error")
	}
}

func TestCleanRelativeRejectsUnsafePaths(t *testing.T) {
	for _, p := range []string{
		"",
		".",
		"..",
		"../escape.md",
		"sub/../../escape.md",
		filepath.Join(string(os.PathSeparator), "absolute.md"),
	} {
		if _, err := paths.CleanRelative(p); err == nil {
			t.Fatalf("expected %q to be rejected", p)
		}
	}
	got, err := paths.CleanRelative("sub/dir/doc.md")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join("sub", "dir", "doc.md") {
		t.Fatalf("cleaned path=%q", got)
	}
}

func TestReadDocumentRejectsBadPathsAndMissingFiles(t *testing.T) {
	c := config.Config{RepositoryPath: t.TempDir(), MaxDocumentBytes: 1000}
	for _, p := range []string{"", "../outside.md", "missing.md"} {
		if _, err := ReadDocument(c, p); err == nil {
			t.Fatalf("expected %q to be rejected", p)
		}
	}
}

func TestScanFailsWhenRepositoryIsMissing(t *testing.T) {
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := config.Config{
		RepositoryPath: filepath.Join(t.TempDir(), "no-such-repo"),
		Extensions:     []string{".md"},
	}
	if _, err := Scan(context.Background(), c, db); err == nil {
		t.Fatal("expected a scan error for a missing repository")
	}
}

func TestScanMatchesExtensionsCaseInsensitivelyAndSkipsTrash(t *testing.T) {
	repo := t.TempDir()
	for name, content := range map[string]string{
		"UPPER.MD":              "# upper",
		".kbm-trash/trashed.md": "# deleted",
		"notes.txt":             "not markdown",
	} {
		path := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// TrashDirectory is excluded even though exclude_directories does not list it.
	c := config.Config{RepositoryPath: repo, Extensions: []string{".md"}, TrashDirectory: ".kbm-trash"}
	result, err := Scan(context.Background(), c, db)
	if err != nil {
		t.Fatal(err)
	}
	if result.Seen != 1 || result.Changed != 1 {
		t.Fatalf("scan=%+v, expected only UPPER.MD", result)
	}
	docs, err := db.Pending(context.Background(), "test-model", "test-model", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Path != "UPPER.MD" {
		t.Fatalf("docs=%+v", docs)
	}
}

func TestHashFailsForMissingFile(t *testing.T) {
	if _, _, err := hashAndSignature(filepath.Join(t.TempDir(), "missing.md")); err == nil {
		t.Fatal("expected a hash error for a missing file")
	}
}

func TestPathTagsDropsReadmeAndDeduplicates(t *testing.T) {
	if got := pathTags("README.md"); len(got) != 0 {
		t.Fatalf("readme should produce no tags, got %v", got)
	}
	got := pathTags("tools/my tool/my_tool.md")
	want := []string{"tools", "my-tool"}
	if len(got) != len(want) {
		t.Fatalf("got=%v want=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got=%v want=%v", got, want)
		}
	}
}

func TestScanStoresFrontmatterAndIssues(t *testing.T) {
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, "good.md"), []byte("---\ntitle: Good\ntags: [alpha]\n---\n# Body\n"), 0644)
	os.WriteFile(filepath.Join(repo, "bad.md"), []byte("---\ntier: golden\n---\n# Body\n"), 0644)
	os.WriteFile(filepath.Join(repo, "broken.md"), []byte("---\ntitle: [\n---\n# Body\n"), 0644)
	os.WriteFile(filepath.Join(repo, "plain.md"), []byte("# No frontmatter\n"), 0644)
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := config.Config{RepositoryPath: repo, Extensions: []string{".md"}, TrashDirectory: ".kbm-trash"}
	if _, err := Scan(context.Background(), c, db); err != nil {
		t.Fatal(err)
	}
	docs, err := db.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]database.Document{}
	for _, d := range docs {
		byPath[d.Path] = d
	}
	if !strings.Contains(byPath["good.md"].FrontmatterJSON, `"title":"Good"`) || byPath["good.md"].FrontmatterIssues != "[]" {
		t.Fatalf("good.md fm=%q issues=%q", byPath["good.md"].FrontmatterJSON, byPath["good.md"].FrontmatterIssues)
	}
	if !strings.Contains(byPath["bad.md"].FrontmatterIssues, "tier") {
		t.Fatalf("bad.md issues=%q", byPath["bad.md"].FrontmatterIssues)
	}
	if !strings.Contains(byPath["broken.md"].FrontmatterIssues, "malformed") {
		t.Fatalf("broken.md issues=%q", byPath["broken.md"].FrontmatterIssues)
	}
	if byPath["plain.md"].FrontmatterJSON != "" || byPath["plain.md"].FrontmatterIssues != "[]" {
		t.Fatalf("plain.md fm=%q issues=%q", byPath["plain.md"].FrontmatterJSON, byPath["plain.md"].FrontmatterIssues)
	}
}

func TestScanSkipsMCPHiddenAndWorkflowPaths(t *testing.T) {
	repo := t.TempDir()
	files := map[string]string{
		"visible.md":                   "# visible",
		".hidden.md":                   "# hidden",
		".drafts/draft.md":             "# draft",
		"logs/run.md":                  "# log",
		"tmp/build.md":                 "# temp",
		"_tmp/stage.md":                "# staged",
		"notes/logs/retained.md":       "# nested logs",
		"notes/_seen.jsonl/ignored.md": "# sidecar subtree",
		"_seen.jsonl":                  "{}",
		"archive/_index.jsonl":         "{}",
		"notes/retained.jsonl":         "{}",
	}
	for name, content := range files {
		path := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := config.Config{
		RepositoryPath: repo,
		Extensions:     []string{".md", ".jsonl"},
	}
	if _, err := Scan(context.Background(), c, db); err != nil {
		t.Fatal(err)
	}
	docs, err := db.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(docs))
	for _, doc := range docs {
		got = append(got, doc.Path)
	}
	want := map[string]bool{
		"visible.md":             true,
		"notes/logs/retained.md": true,
		"notes/retained.jsonl":   true,
	}
	if len(got) != len(want) {
		t.Fatalf("scanned paths=%v, want exactly %v", got, want)
	}
	for _, path := range got {
		if !want[path] {
			t.Errorf("unexpected scanned path %q", path)
		}
	}
}

func TestScanHonoursExcludePathsPatternsAndExtensions(t *testing.T) {
	repo := t.TempDir()
	files := map[string]string{
		"docs/tmp/ignored.md":   "# x",
		"docs/tmp/deep/y.md":    "# x",
		"docs/keep.md":          "# keep",
		"other/tmp/retained.md": "# retained",
		"drafts/a.md":           "# draft",
		"drafts/deep/b.md":      "# draft",
		"note.draft.md":         "# draft",
		"sub/note.draft.md":     "# draft",
		"data/nested/secret.md": "# secret",
		"report.csv":            "# csv",
		"data/table.csv":        "# csv",
		"ok.md":                 "# ok",
	}
	for name, content := range files {
		path := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := config.Config{
		RepositoryPath:    repo,
		Extensions:        []string{".md", ".csv"},
		Exclude:           []string{"docs/tmp", "drafts/**", "**/*.draft.md", "**/secret.md"},
		ExcludeExtensions: []string{".CSV"},
		TrashDirectory:    ".kbm-trash",
	}
	result, err := Scan(context.Background(), c, db)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := db.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, d := range docs {
		got[d.Path] = true
	}
	want := map[string]bool{
		"docs/keep.md":          true,
		"other/tmp/retained.md": true,
		"ok.md":                 true,
	}
	if len(got) != len(want) || result.Seen != len(want) {
		t.Fatalf("scanned paths=%v seen=%d, want %v", got, result.Seen, want)
	}
	for path := range got {
		if !want[path] {
			t.Errorf("unexpected scanned path %q", path)
		}
	}
}

func TestScanRejectsInvalidExcludePattern(t *testing.T) {
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := config.Config{
		RepositoryPath: t.TempDir(),
		Extensions:     []string{".md"},
		Exclude:        []string{"../escape"},
	}
	if _, err := Scan(context.Background(), c, db); err == nil {
		t.Fatal("expected an invalid exclude pattern to fail the scan")
	}
}

func TestSiblingFilesHidesExcludedNamesAndExtensions(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, "runbooks")
	os.MkdirAll(dir, 0755)
	os.WriteFile(filepath.Join(dir, "deploy.md"), []byte("# Deploy"), 0644)
	os.WriteFile(filepath.Join(dir, "other.md"), []byte("# Other"), 0644)
	os.WriteFile(filepath.Join(dir, "deploy.sh"), []byte("#!/bin/sh\n"), 0644)
	os.WriteFile(filepath.Join(dir, "secrets.txt"), []byte("hidden"), 0644)
	os.WriteFile(filepath.Join(dir, "data.csv"), []byte("a,b"), 0644)
	c := config.Config{
		RepositoryPath:    repo,
		Extensions:        []string{".md"},
		Exclude:           []string{"**/secrets.txt"},
		ExcludeExtensions: []string{".csv"},
	}
	got, err := SiblingFiles(c, "runbooks/deploy.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "deploy.sh" {
		t.Fatalf("siblings=%v, expected only deploy.sh", got)
	}
}

func TestSiblingFilesListsNonDocumentNeighbours(t *testing.T) {
	repo := t.TempDir()
	dir := filepath.Join(repo, "runbooks")
	os.MkdirAll(dir, 0755)
	os.WriteFile(filepath.Join(dir, "deploy.md"), []byte("# Deploy"), 0644)
	os.WriteFile(filepath.Join(dir, "other.md"), []byte("# Other"), 0644)
	os.WriteFile(filepath.Join(dir, "deploy.sh"), []byte("#!/bin/sh\n"), 0644)
	os.WriteFile(filepath.Join(dir, ".hidden"), []byte("x"), 0644)
	os.MkdirAll(filepath.Join(dir, "subdir"), 0755)
	c := config.Config{RepositoryPath: repo, Extensions: []string{".md"}}
	got, err := SiblingFiles(c, "runbooks/deploy.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "deploy.sh" {
		t.Fatalf("siblings=%v", got)
	}
}

func TestSiblingFilesRejectsTraversal(t *testing.T) {
	c := config.Config{RepositoryPath: t.TempDir(), Extensions: []string{".md"}}
	if _, err := SiblingFiles(c, "../escape.md"); err == nil {
		t.Fatal("expected traversal rejection")
	}
}

func TestScanIgnoresManagedBlockChanges(t *testing.T) {
	repo := t.TempDir()
	md := filepath.Join(repo, "a.md")
	os.WriteFile(md, []byte("---\ntitle: A\n---\n# Alpha\n"), 0644)
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := config.Config{RepositoryPath: repo, Extensions: []string{".md"}, TrashDirectory: ".kbm-trash"}
	first, err := Scan(context.Background(), c, db)
	if err != nil || first.Changed != 1 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	// A managed-block update rewrites the file bytes but must not count as a
	// content change: no requeue, no suggestion churn.
	os.WriteFile(md, []byte("---\ntitle: A\nkbm:\n  link_check:\n    at: \"2026-10-20\"\n    dead: [https://x.example]\n---\n# Alpha\n"), 0644)
	second, err := Scan(context.Background(), c, db)
	if err != nil || second.Changed != 0 {
		t.Fatalf("kbm update must not mark content changed: %+v err=%v", second, err)
	}
	pending, err := db.Pending(context.Background(), "m", "m", false)
	if err != nil || len(pending) != 1 {
		// The doc was never analyzed, so it is still pending — but for the
		// original content, not the kbm update. Compare the stored hash.
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	// A real content edit alongside still counts as changed.
	os.WriteFile(md, []byte("---\ntitle: A\nkbm:\n  link_check:\n    at: \"2026-10-21\"\n---\n# Alpha revised\n"), 0644)
	third, err := Scan(context.Background(), c, db)
	if err != nil || third.Changed != 1 {
		t.Fatalf("real change must be detected: %+v err=%v", third, err)
	}
}

func TestScanFlagsUnresolvedInternalLinks(t *testing.T) {
	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, "sub"), 0755)
	os.WriteFile(filepath.Join(repo, "a.md"), []byte("# A\n\nSee [b](sub/b.md), [gone](missing.md), [asset](asset.txt), [anchor](#x).\n"), 0644)
	os.WriteFile(filepath.Join(repo, "sub", "b.md"), []byte("# B\n"), 0644)
	os.WriteFile(filepath.Join(repo, "asset.txt"), []byte("data"), 0644)
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := config.Config{RepositoryPath: repo, Extensions: []string{".md"}, TrashDirectory: ".kbm-trash"}
	if _, err := Scan(context.Background(), c, db); err != nil {
		t.Fatal(err)
	}
	docs, err := db.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var aDoc, bDoc *database.Document
	for i := range docs {
		switch docs[i].Path {
		case "a.md":
			aDoc = &docs[i]
		case "sub/b.md":
			bDoc = &docs[i]
		}
	}
	if aDoc == nil || bDoc == nil {
		t.Fatalf("docs=%+v", docs)
	}
	if !strings.Contains(aDoc.LinkIssues, "missing.md") {
		t.Fatalf("link_issues=%q", aDoc.LinkIssues)
	}
	if strings.Contains(aDoc.LinkIssues, "b.md") || strings.Contains(aDoc.LinkIssues, "asset.txt") {
		t.Fatalf("resolved links must not be flagged: %q", aDoc.LinkIssues)
	}
	if bDoc.LinkIssues != "[]" && bDoc.LinkIssues != "" {
		t.Fatalf("clean doc should have no issues: %q", bDoc.LinkIssues)
	}
}

// BenchmarkScanRescan measures the hot path the engineering-memory notes call
// out: every scan re-reads and re-hashes each document even when nothing
// changed, so the steady-state cost is the walk + canonical hash per file.
func BenchmarkScanRescan(b *testing.B) {
	repo := b.TempDir()
	for i := 0; i < 50; i++ {
		dir := filepath.Join(repo, "section")
		os.MkdirAll(dir, 0755)
		content := "---\ntags: [tool]\n---\n# Doc\n\n" + strings.Repeat("body line\n", 40)
		os.WriteFile(filepath.Join(dir, strings.Repeat("d", i%9+1)+".md"), []byte(content), 0644)
	}
	db, err := database.Open(b.Context(), filepath.Join(b.TempDir(), "km.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	c := config.Config{RepositoryPath: repo, Extensions: []string{".md"}, TrashDirectory: ".kbm-trash", MaxDocumentBytes: 100000}
	if _, err := Scan(b.Context(), c, db); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		if _, err := Scan(b.Context(), c, db); err != nil {
			b.Fatal(err)
		}
	}
}
