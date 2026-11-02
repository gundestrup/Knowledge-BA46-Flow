package main

import (
	"os"
	"path/filepath"
	"testing"

	"knowledge-ba46-flow/internal/database"
)

// Binary-level e2e: the real CLI drives scan -> analyze -> incremental
// re-analysis -> deletion handling against a stub Ollama server, and each
// stage is verified against expected database state — not merely exit codes.
func TestE2EIncrementalLifecycle(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	writeFile := func(name, content string) {
		p := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	writeFile("alpha.md", "# Alpha\n\nFirst note body.\n")
	writeFile("beta.md", "# Beta\n\nSecond note body.\n")

	srv := taskModelServer(t, map[string]string{
		"test": `{"title":"Note","summary":"s","tags":["x"],"obsolete":false,"suggestions":[]}`,
	})
	defer srv.Close()
	configPath := writeCLIConfig(t, dir, map[string]any{"ollama_url": srv.URL})

	openDB := func() *database.DB {
		db, err := database.Open(t.Context(), filepath.Join(dir, "data", "knowledge.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		return db
	}
	pending := func() int {
		docs, err := openDB().Pending(t.Context(), "test", "", false)
		if err != nil {
			t.Fatal(err)
		}
		return len(docs)
	}
	docStatus := func(path string) string {
		docs, err := openDB().Documents(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range docs {
			if d.Path == path {
				return d.Status
			}
		}
		return ""
	}

	// Scan detects both documents as pending work.
	if err := runCLI("scan", "--config", configPath); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := pending(); got != 2 {
		t.Fatalf("after scan pending=%d, want 2", got)
	}

	// Analyze stamps both — a repeat run must find nothing to do.
	if err := runCLI("analyze", "--config", configPath); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if got := pending(); got != 0 {
		t.Fatalf("after analyze pending=%d, want 0", got)
	}
	if got := docStatus("alpha.md"); got != "analyzed" {
		t.Fatalf("alpha status=%q", got)
	}
	if err := runCLI("analyze", "--config", configPath); err != nil {
		t.Fatalf("re-analyze: %v", err)
	}
	if got := pending(); got != 0 {
		t.Fatalf("repeat analyze re-queued %d docs — stamps should prevent it", got)
	}

	// A body edit re-queues only the changed document.
	writeFile("alpha.md", "# Alpha\n\nEdited body with new facts.\n")
	if err := runCLI("scan", "--config", configPath); err != nil {
		t.Fatal(err)
	}
	if got := pending(); got != 1 {
		t.Fatalf("after edit pending=%d, want 1 (only alpha)", got)
	}
	if err := runCLI("analyze", "--config", configPath); err != nil {
		t.Fatal(err)
	}
	if got := pending(); got != 0 {
		t.Fatalf("after delta analyze pending=%d", got)
	}

	// Deletion tombstones the document instead of losing its lineage.
	if err := os.Remove(filepath.Join(repo, "beta.md")); err != nil {
		t.Fatal(err)
	}
	if err := runCLI("scan", "--config", configPath); err != nil {
		t.Fatal(err)
	}
	if got := docStatus("beta.md"); got != "missing" {
		t.Fatalf("beta status=%q after delete, want missing", got)
	}
	if got := pending(); got != 0 {
		t.Fatalf("deletion should not re-queue analysis: pending=%d", got)
	}
}
