package linkcheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/scanner"
)

func TestRunRecordsVerdictsWithoutChangingContentHash(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dead":
			w.WriteHeader(http.StatusNotFound)
		case "/walled":
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer live.Close()

	repo := t.TempDir()
	doc := "---\ntitle: A\n---\n# Alpha\n\n" +
		"See [ok](" + live.URL + "/ok), [dead](" + live.URL + "/dead) and [walled](" + live.URL + "/walled).\n"
	os.WriteFile(filepath.Join(repo, "a.md"), []byte(doc), 0644)
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := config.Config{RepositoryPath: repo, Extensions: []string{".md"}, TrashDirectory: ".kbm-trash"}
	if _, err := scanner.Scan(context.Background(), c, db); err != nil {
		t.Fatal(err)
	}
	docs, _ := db.Documents(context.Background())
	before := docs[0].SHA256

	r, err := Run(context.Background(), c, db)
	if err != nil {
		t.Fatal(err)
	}
	if r.URLs != 3 || r.Dead != 1 || r.Unverified != 1 || r.Written != 1 {
		t.Fatalf("result=%+v", r)
	}

	// The file gained a managed block; the canonical hash must not move.
	raw, err := os.ReadFile(filepath.Join(repo, "a.md"))
	if err != nil {
		t.Fatal(err)
	}
	fields, _, err := frontmatter.Parse(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	kbm, ok := fields[frontmatter.ManagedKey].(map[string]any)
	if !ok {
		t.Fatalf("kbm block missing: %v", fields)
	}
	lc, _ := kbm["link_check"].(map[string]any)
	if lc["at"] == "" {
		t.Fatalf("link_check=%v", lc)
	}
	dead, _ := lc["dead"].([]any)
	unverified, _ := lc["unverified"].([]any)
	if len(dead) != 1 || !strings.Contains(dead[0].(string), "/dead") {
		t.Fatalf("dead=%v", dead)
	}
	if len(unverified) != 1 || !strings.Contains(unverified[0].(string), "/walled") {
		t.Fatalf("unverified=%v", unverified)
	}

	// DB reflects the new frontmatter and the content hash is untouched.
	if _, err := scanner.Scan(context.Background(), c, db); err != nil {
		t.Fatal(err)
	}
	docs, _ = db.Documents(context.Background())
	if docs[0].SHA256 != before {
		t.Fatalf("content hash moved by managed write: %s -> %s", before, docs[0].SHA256)
	}
	if !strings.Contains(docs[0].FrontmatterJSON, "link_check") {
		t.Fatalf("stored frontmatter not refreshed: %s", docs[0].FrontmatterJSON)
	}
}

func TestRunNoExternalURLs(t *testing.T) {
	repo := t.TempDir()
	os.WriteFile(filepath.Join(repo, "a.md"), []byte("# Alpha\n"), 0644)
	db, err := database.Open(t.Context(), filepath.Join(t.TempDir(), "km.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := config.Config{RepositoryPath: repo, Extensions: []string{".md"}, TrashDirectory: ".kbm-trash"}
	if _, err := scanner.Scan(context.Background(), c, db); err != nil {
		t.Fatal(err)
	}
	r, err := Run(context.Background(), c, db)
	if err != nil || r.URLs != 0 || r.Written != 0 {
		t.Fatalf("result=%+v err=%v", r, err)
	}
}

func TestResultSummary(t *testing.T) {
	r := Result{URLs: 10, Documents: 4, Dead: 2, Unverified: 3, Written: 4}
	got := r.Summary()
	for _, part := range []string{"10", "4", "2", "3"} {
		if !strings.Contains(got, part) {
			t.Fatalf("summary %q missing %q", got, part)
		}
	}
}
