package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/paths"
)

func TestTrackedLLMEvalManifestIsAPathAndHashAllowlist(t *testing.T) {
	manifestPath := filepath.Join("..", "..", "example-knowledge", "llm-eval", "selection.json")
	manifest, err := readManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Documents) != 14 {
		t.Fatalf("selected %d notes, want 14", len(manifest.Documents))
	}
	if manifest.Taxonomy == nil || manifest.Taxonomy.Path != taxonomyFileName ||
		len(manifest.Taxonomy.SHA256) != sha256.Size*2 {
		t.Fatalf("real eval must pin its tag taxonomy sidecar: %+v", manifest.Taxonomy)
	}
	expectedBytes, err := os.ReadFile(filepath.Join(filepath.Dir(manifestPath), "expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expected struct {
		Documents int `json:"documents"`
	}
	if err := json.Unmarshal(expectedBytes, &expected); err != nil {
		t.Fatal(err)
	}
	if expected.Documents != len(manifest.Documents) {
		t.Fatalf("expected rubric covers %d notes; selection contains %d", expected.Documents, len(manifest.Documents))
	}
	seen := make(map[string]bool, len(manifest.Documents))
	for _, document := range manifest.Documents {
		relative, err := paths.CleanRelative(document.Path)
		if err != nil || filepath.Ext(relative) != ".md" {
			t.Errorf("invalid real-note path %q: %v", document.Path, err)
		}
		if seen[document.Path] {
			t.Errorf("duplicate path %q", document.Path)
		}
		seen[document.Path] = true
		if _, err := hex.DecodeString(document.SHA256); err != nil || len(document.SHA256) != sha256.Size*2 {
			t.Errorf("invalid SHA-256 for %s: %q", document.Path, document.SHA256)
		}
		if strings.TrimSpace(document.Case) == "" {
			t.Errorf("missing test case description for %s", document.Path)
		}
	}
}

func TestPrepareCopiesHashPinnedPublicNotesAndDeniesWrites(t *testing.T) {
	sourceRoot := t.TempDir()
	sourcePath := filepath.Join(sourceRoot, "languages", "go.md")
	if err := os.MkdirAll(filepath.Dir(sourcePath), 0755); err != nil {
		t.Fatal(err)
	}
	content := []byte("# Go\n\nA compiled language.\n")
	if err := os.WriteFile(sourcePath, content, 0644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	manifestPath := writeManifest(t, sourceRecord{
		Path: "languages/go.md", SHA256: hex.EncodeToString(sum[:]), Case: "language",
	})
	destination := filepath.Join(t.TempDir(), "corpus")
	copied, err := prepare(sourceRoot, manifestPath, destination)
	if err != nil {
		t.Fatal(err)
	}
	if len(copied) != 1 || copied[0].Path != "languages/go.md" || copied[0].SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("copied=%+v", copied)
	}
	got, err := os.ReadFile(filepath.Join(destination, "languages", "go.md"))
	if err != nil || string(got) != string(content) {
		t.Fatalf("copied source=%q error=%v", got, err)
	}
	policy, err := os.ReadFile(filepath.Join(destination, ".kb-policy.json"))
	if err != nil || strings.TrimSpace(string(policy)) != `{"mutations":"deny"}` {
		t.Fatalf("write policy=%q error=%v", policy, err)
	}
}

func TestPrepareCopiesPinnedTaxonomySidecar(t *testing.T) {
	sourceRoot := t.TempDir()
	note := []byte("# Ruby\n\nA programming language.\n")
	if err := os.MkdirAll(filepath.Join(sourceRoot, "languages"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "languages", "ruby.md"), note, 0644); err != nil {
		t.Fatal(err)
	}
	taxonomyBytes := []byte(`version: 1
kind: [programming-language, framework, library, database, cms, tool]
subject:
  languages: [ruby]
function: [testing]
`)
	if err := os.WriteFile(filepath.Join(sourceRoot, taxonomyFileName), taxonomyBytes, 0600); err != nil {
		t.Fatal(err)
	}
	noteHash := sha256.Sum256(note)
	taxonomyHash := sha256.Sum256(taxonomyBytes)
	manifestBytes, err := json.Marshal(sourceManifest{
		SchemaVersion: 1,
		Taxonomy:      &sourceRecord{Path: taxonomyFileName, SHA256: hex.EncodeToString(taxonomyHash[:])},
		Documents:     []sourceRecord{{Path: "languages/ruby.md", SHA256: hex.EncodeToString(noteHash[:])}},
	})
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(t.TempDir(), "selection.json")
	if err := os.WriteFile(manifestPath, manifestBytes, 0600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "corpus")
	copied, err := prepare(sourceRoot, manifestPath, destination)
	if err != nil {
		t.Fatal(err)
	}
	if len(copied) != 2 || copied[1].Path != taxonomyFileName {
		t.Fatalf("copied inputs=%+v", copied)
	}
	got, err := os.ReadFile(filepath.Join(destination, taxonomyFileName))
	if err != nil || string(got) != string(taxonomyBytes) {
		t.Fatalf("taxonomy sidecar=%q error=%v", got, err)
	}
}

func TestPrepareRejectsUnpinnedPrivateAndUnsafeSources(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		content []byte
		hash    string
		want    string
	}{
		{
			name: "hash drift",
			path: "notes/a.md", content: []byte("# Changed\n"), hash: strings.Repeat("0", 64),
			want: "source changed since curation",
		},
		{
			name: "private policy",
			path: "notes/a.md", content: []byte("---\nkb_policy:\n  no_llm_context: true\n---\n# Private\n"),
			want: "private or has invalid frontmatter policy",
		},
		{
			name: "credential pattern",
			path: "notes/a.md", content: []byte("# Token example\n\nsk-123456789012345678901234567890\n"),
			want: "credential-like material",
		},
		{
			name: "path traversal",
			path: "../outside.md", content: []byte("# Unsafe\n"), hash: strings.Repeat("0", 64),
			want: "unsafe source path",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sourceRoot := t.TempDir()
			if !strings.Contains(tc.path, "..") {
				path := filepath.Join(sourceRoot, filepath.FromSlash(tc.path))
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, tc.content, 0644); err != nil {
					t.Fatal(err)
				}
			}
			hash := tc.hash
			if hash == "" {
				sum := sha256.Sum256(tc.content)
				hash = hex.EncodeToString(sum[:])
			}
			manifestPath := writeManifest(t, sourceRecord{Path: tc.path, SHA256: hash})
			_, err := prepare(sourceRoot, manifestPath, filepath.Join(t.TempDir(), "corpus"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("prepare error=%v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestPrepareRejectsManifestAndSourceEdgeCases(t *testing.T) {
	note := []byte("# Note\n")
	noteSum := sha256.Sum256(note)
	noteHash := hex.EncodeToString(noteSum[:])
	record := sourceRecord{Path: "notes/a.md", SHA256: noteHash}

	// readManifest input failures.
	badManifest := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(badManifest, []byte(`{"schema_version":2,"documents":[{"path":"x.md"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readManifest(badManifest); err == nil {
		t.Error("unsupported schema version should fail")
	}
	if _, err := readManifest(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Error("missing manifest should fail")
	}
	emptyManifest := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(emptyManifest, []byte(`{"schema_version":1,"documents":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readManifest(emptyManifest); err == nil {
		t.Error("empty documents should fail")
	}

	// Duplicate manifest paths fail before any copy.
	dupManifest := filepath.Join(t.TempDir(), "dup.json")
	dupBytes, err := json.Marshal(sourceManifest{SchemaVersion: 1, Documents: []sourceRecord{record, record}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dupManifest, dupBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	sourceRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sourceRoot, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "notes", "a.md"), note, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepare(sourceRoot, dupManifest, filepath.Join(t.TempDir(), "corpus")); err == nil ||
		!strings.Contains(err.Error(), "duplicate source path") {
		t.Errorf("duplicate path: %v", err)
	}

	// Non-Markdown sources are refused.
	txt := []byte("not a note")
	txtSum := sha256.Sum256(txt)
	if err := os.WriteFile(filepath.Join(sourceRoot, "notes", "b.txt"), txt, 0o600); err != nil {
		t.Fatal(err)
	}
	txtManifest := writeManifest(t, sourceRecord{Path: "notes/b.txt", SHA256: hex.EncodeToString(txtSum[:])})
	if _, err := prepare(sourceRoot, txtManifest, filepath.Join(t.TempDir(), "corpus")); err == nil ||
		!strings.Contains(err.Error(), "must be Markdown") {
		t.Errorf("non-markdown source: %v", err)
	}

	// A directory where a document is expected is not a regular file.
	if err := os.MkdirAll(filepath.Join(sourceRoot, "notes", "dir.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	dirManifest := writeManifest(t, sourceRecord{Path: "notes/dir.md", SHA256: strings.Repeat("0", 64)})
	if _, err := prepare(sourceRoot, dirManifest, filepath.Join(t.TempDir(), "corpus")); err == nil ||
		!strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("directory source: %v", err)
	}
}

func TestPrepareRejectsBadTaxonomySidecar(t *testing.T) {
	sourceRoot := t.TempDir()
	note := []byte("# Ruby\n")
	if err := os.MkdirAll(filepath.Join(sourceRoot, "languages"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, "languages", "ruby.md"), note, 0o600); err != nil {
		t.Fatal(err)
	}
	noteSum := sha256.Sum256(note)
	manifestFor := func(taxonomy sourceRecord) string {
		data, err := json.Marshal(sourceManifest{
			SchemaVersion: 1,
			Taxonomy:      &taxonomy,
			Documents:     []sourceRecord{{Path: "languages/ruby.md", SHA256: hex.EncodeToString(noteSum[:])}},
		})
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "selection.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// The sidecar path must be exactly taxonomy.yaml.
	if _, err := prepare(sourceRoot, manifestFor(sourceRecord{
		Path: "docs/taxonomy.yaml", SHA256: strings.Repeat("0", 64),
	}), filepath.Join(t.TempDir(), "corpus")); err == nil ||
		!strings.Contains(err.Error(), "must be "+taxonomyFileName) {
		t.Errorf("wrong sidecar path: %v", err)
	}

	// Hash drift on the sidecar fails like a document drift.
	if err := os.WriteFile(filepath.Join(sourceRoot, taxonomyFileName), []byte("version: 1\nkind: [tool]\nsubject: {x: [y]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepare(sourceRoot, manifestFor(sourceRecord{
		Path: taxonomyFileName, SHA256: strings.Repeat("0", 64),
	}), filepath.Join(t.TempDir(), "corpus")); err == nil ||
		!strings.Contains(err.Error(), "changed since curation") {
		t.Errorf("sidecar hash drift: %v", err)
	}

	// An invalid taxonomy file is refused even when the hash pins it.
	badTaxonomy := []byte("version: 9\n")
	if err := os.WriteFile(filepath.Join(sourceRoot, taxonomyFileName), badTaxonomy, 0o600); err != nil {
		t.Fatal(err)
	}
	badSum := sha256.Sum256(badTaxonomy)
	if _, err := prepare(sourceRoot, manifestFor(sourceRecord{
		Path: taxonomyFileName, SHA256: hex.EncodeToString(badSum[:]),
	}), filepath.Join(t.TempDir(), "corpus")); err == nil ||
		!strings.Contains(err.Error(), "validate "+taxonomyFileName) {
		t.Errorf("invalid sidecar content: %v", err)
	}
}

func writeManifest(t *testing.T, document sourceRecord) string {
	t.Helper()
	data, err := json.Marshal(sourceManifest{SchemaVersion: 1, Documents: []sourceRecord{document}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "selection.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
