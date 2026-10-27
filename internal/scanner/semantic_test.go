package scanner

import (
	"os"
	"path/filepath"
	"testing"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
)

func TestScannerSemanticHashMatchesLiveDocumentProjection(t *testing.T) {
	content := "---\ndocument_id: \"18aa5712-f045-436c-92c6-443247888962\"\ntitle: AEM\nkbm:\n  freshness: {verdict: current}\nrelated: [old.md]\ntags: [cms]\n---\nRead [guide](../old.md#top).\n"
	hashes, _, err := hashAndSignatureContent(content)
	if err != nil {
		t.Fatal(err)
	}
	if hashes.semantic != frontmatter.SemanticHash(content) {
		t.Fatalf("scanner semantic=%s, live semantic=%s", hashes.semantic, frontmatter.SemanticHash(content))
	}
	if frontmatter.SemanticHash(content) != frontmatter.SemanticHash(frontmatter.Canonical(content)) {
		t.Fatal("canonicalizing managed metadata changed semantic hash")
	}
}

func TestGraphOnlyEditsKeepTaskLineagesButSupersedeFullFileProposals(t *testing.T) {
	c, db, write := identityScanFixture(t)
	c.Extensions = []string{".md"}
	write("old.md", "# Old target\n")
	write("new.md", "# New target\n")
	write("source.md", "---\ntitle: Source\nrelated: [old.md]\ntags: [guide]\n---\nSee [the guide](old.md#start).\n")
	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	doc := scannerDoc(t, db, "source.md")
	vector := []byte{0, 0, 128, 63}
	if err := db.SaveExternalAnalysis(t.Context(), doc, "same-model", "Source", "summary", []string{"guide"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveEnrichment(t.Context(), doc, "fast-model", "Source", "summary", []string{"guide"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveTask(t.Context(), doc, "expand", "same-model", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveTask(t.Context(), doc, "freshness", "same-model", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveEmbedding(t.Context(), doc, "embed-model", vector); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(t.Context(), doc, "same-model", "Source", "summary", []string{"guide"}, []database.Suggestion{{
		Action: "expand", Origin: "analysis", Reason: "review snapshot", ProposedContent: "# Source\n\nReviewed addition.\n",
	}}); err != nil {
		t.Fatal(err)
	}

	write("source.md", "---\ntitle: Source\nrelated: [new.md]\ntags: [guide]\n---\nSee [the guide](new.md#changed).\n")
	result, err := Scan(t.Context(), c, db)
	if err != nil || result.Changed != 1 {
		t.Fatalf("scan=%+v err=%v", result, err)
	}
	updated := scannerDoc(t, db, "source.md")
	if updated.SemanticSHA256 != doc.SemanticSHA256 || updated.SHA256 == doc.SHA256 {
		t.Fatalf("graph-only edit hashes: semantic %s -> %s; source %s -> %s",
			doc.SemanticSHA256, updated.SemanticSHA256, doc.SHA256, updated.SHA256)
	}
	if pending, err := db.Pending(t.Context(), "same-model", "same-model", false); err != nil || hasPath(pending, "source.md") {
		t.Fatalf("local analysis was requeued by graph-only edit: %v %v", pending, err)
	}
	if pending, err := db.PendingExternal(t.Context(), "same-model", "same-model", false, false); err != nil || hasPath(pending, "source.md") {
		t.Fatalf("external analysis was requeued by graph-only edit: %v %v", pending, err)
	}
	if pending, err := db.PendingEnrich(t.Context(), "fast-model", false); err != nil || hasPath(pending, "source.md") {
		t.Fatalf("enrichment was requeued by graph-only edit: %v %v", pending, err)
	}
	for _, task := range []string{config.TaskExpand, config.TaskFreshness, config.TaskEmbed} {
		model := "same-model"
		if task == config.TaskEmbed {
			model = "embed-model"
		}
		pending, err := db.PendingTask(t.Context(), task, model, false)
		if err != nil || hasPath(pending, "source.md") {
			t.Fatalf("%s was requeued by graph-only edit: %v %v", task, pending, err)
		}
	}
	suggestions, err := db.Suggestions(t.Context(), "superseded")
	if err != nil {
		t.Fatal(err)
	}
	var retired bool
	for _, suggestion := range suggestions {
		if suggestion.DocumentPath == "source.md" && suggestion.Reason == "review snapshot" {
			retired = true
		}
	}
	if !retired {
		t.Fatal("full-file proposal was not retired when its strict source hash changed")
	}
}

func TestLegacyCurrentLineageIsSeededWithoutCorpusReanalysis(t *testing.T) {
	c, db, write := identityScanFixture(t)
	c.Extensions = []string{".md"}
	raw := "---\ntitle: A\nrelated: [b.md]\n---\n# A\n\nSame content.\n"
	write("a.md", raw)
	old := database.Document{
		Path: "a.md", SHA256: CanonicalHash(raw), Size: int64(len(raw)),
		FrontmatterJSON: `{"title":"A","related":["b.md"]}`, FrontmatterIssues: "[]",
		FmSHA256: "legacy-fm", BodySHA256: "legacy-body",
	}
	if _, err := db.UpsertScanned(t.Context(), old, "legacy-scan"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAnalysis(t.Context(), old, "same-model", "A", "summary", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	if pending, err := db.Pending(t.Context(), "same-model", "same-model", false); err != nil || len(pending) != 0 {
		t.Fatalf("current legacy analysis should seed semantic lineage, not re-run: %v %v", pending, err)
	}
	doc := scannerDoc(t, db, "a.md")
	if doc.AnalyzedSHA256 != doc.AnalysisInputSHA256 || doc.AnalyzedSemanticSHA256 != doc.SemanticSHA256 {
		t.Fatalf("legacy lineage was not safely migrated: %+v", doc)
	}
}

func TestPromptChangesInvalidateOnlyTheAffectedTask(t *testing.T) {
	c, db, write := identityScanFixture(t)
	c.Extensions = []string{".md"}
	c.PromptDirectory = filepath.Join(t.TempDir(), "prompts")
	c.MCPServer = config.MCPServerKnowledgeBase
	if err := os.MkdirAll(c.PromptDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	promptFiles := map[string]string{
		"base.md":                      "base v1",
		"knowledge-base-mcp-server.md": "MCP rules",
		"analyze.md":                   "analyze v1",
		"enrich.md":                    "enrich v1",
		"expand.md":                    "expand v1",
		"freshness.md":                 "freshness v1",
		"freshness-verify.md":          "verify v1",
	}
	for name, content := range promptFiles {
		if err := os.WriteFile(filepath.Join(c.PromptDirectory, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.md", "---\ntitle: A\ntags: [guide]\n---\n# A\n\nBody.\n")
	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	doc := scannerDoc(t, db, "a.md")
	if err := db.SaveAnalysis(t.Context(), doc, "same-model", "A", "summary", []string{"guide"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveTask(t.Context(), doc, config.TaskFreshness, "same-model", nil); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(c.PromptDirectory, "analyze.md"), []byte("analyze v2"), 0600); err != nil {
		t.Fatal(err)
	}
	if scan, err := Scan(t.Context(), c, db); err != nil || scan.Changed != 0 {
		t.Fatalf("prompt-only scan=%+v err=%v", scan, err)
	}
	if pending, err := db.Pending(t.Context(), "same-model", "same-model", false); err != nil || len(pending) != 1 {
		t.Fatalf("analysis prompt change was not queued: %v %v", pending, err)
	}
	if pending, err := db.PendingTask(t.Context(), config.TaskFreshness, "same-model", false); err != nil || len(pending) != 0 {
		t.Fatalf("analysis prompt change incorrectly queued freshness: %v %v", pending, err)
	}

	if err := os.WriteFile(filepath.Join(c.PromptDirectory, "freshness.md"), []byte("freshness v2"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	if pending, err := db.PendingTask(t.Context(), config.TaskFreshness, "same-model", false); err != nil || len(pending) != 1 {
		t.Fatalf("freshness prompt change was not queued: %v %v", pending, err)
	}
}

func hasPath(docs []database.Document, path string) bool {
	for _, doc := range docs {
		if doc.Path == path {
			return true
		}
	}
	return false
}

func scannerDoc(t *testing.T, db *database.DB, path string) database.Document {
	t.Helper()
	docs, err := db.Documents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range docs {
		if doc.Path == path {
			return doc
		}
	}
	t.Fatalf("document %s not found", path)
	return database.Document{}
}
