package scanner

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	doc = scannerDoc(t, db, "a.md")
	doc.FreshnessWebVerify = true
	doc.FreshnessVerifyModel = "verify-model"
	doc.FreshnessVerifyPromptSHA = "verify v1"
	if err := db.StampTask(t.Context(), doc, config.TaskFreshness, "same-model"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(c.PromptDirectory, "freshness-verify.md"), []byte("verify v2"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	if pending, err := db.PendingTask(t.Context(), config.TaskFreshness, "same-model", false); err != nil || len(pending) != 0 {
		t.Fatalf("verify-only prompt edit queued base freshness without opt-in: %v %v", pending, err)
	}
	verifyPending, err := db.PendingTaskForRun(t.Context(), config.TaskFreshness, "same-model", false,
		database.TaskRunOptions{WebVerify: true, VerifyModel: "verify-model", VerifyPromptSHA: "verify v2"})
	if err != nil || len(verifyPending) != 1 {
		t.Fatalf("verify prompt edit did not queue opt-in verification only: %v %v", verifyPending, err)
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

// TestScanScopesTaxonomyInvalidationToAffectedDocuments proves a taxonomy
// edit only moves the input fingerprints of documents whose taxonomy
// projection (path-derived + current tags) actually changed.
func TestScanScopesTaxonomyInvalidationToAffectedDocuments(t *testing.T) {
	c, db, write := identityScanFixture(t)
	base := `version: 1
kind: [tool, programming-language, pattern]
subject:
  languages:
    - go
  topics:
    - taxonomy
function: [testing]
`
	write("taxonomy.yaml", base)
	write("tools/x.md", "---\ntags: [tool]\n---\n# X\n\nOne.\n\nTwo.\n\nThree.\n\nFour.\n\nFive.\n")
	write("languages/go.md", "---\ntags: [programming-language]\n---\n# Go\n\nOne.\n\nTwo.\n\nThree.\n\nFour.\n\nFive.\n")
	write("languages/zig.md", "---\ntags: [programming-language]\n---\n# Zig\n\nOne.\n\nTwo.\n\nThree.\n\nFour.\n\nFive.\n")
	write("tools/STUB.md", "# Template\n")
	write("tools/placeholder.md", "---\ntags: [stub, tool]\n---\n# Placeholder\n")
	write("tools/private.md", "---\ntags: [tool]\nkb_policy: {no_llm_context: true}\n---\n# Private\n")
	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	inputs := map[string]string{}
	for _, p := range []string{
		"tools/x.md", "languages/go.md", "languages/zig.md",
		"tools/placeholder.md", "tools/private.md",
	} {
		inputs[p] = scannerDoc(t, db, p).AnalysisInputSHA256
	}
	write("taxonomy.yaml", `version: 1
kind: [tool, programming-language, pattern]
subject:
  languages:
    - go
    - zig
  topics:
    - taxonomy
function: [testing]
`)
	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	if got := scannerDoc(t, db, "tools/x.md").AnalysisInputSHA256; got != inputs["tools/x.md"] {
		t.Fatal("unrelated subject addition moved an unaffected document's fingerprint")
	}
	if got := scannerDoc(t, db, "languages/go.md").AnalysisInputSHA256; got != inputs["languages/go.md"] {
		t.Fatal("sibling-language addition moved an unaffected document's fingerprint")
	}
	if got := scannerDoc(t, db, "languages/zig.md").AnalysisInputSHA256; got == inputs["languages/zig.md"] {
		t.Fatal("newly allowed path-derived subject did not move the document's fingerprint")
	}
	for _, p := range []string{"tools/placeholder.md", "tools/private.md"} {
		if got := scannerDoc(t, db, p).AnalysisInputSHA256; got != inputs[p] {
			t.Fatalf("taxonomy edit changed generation input for skipped document %s", p)
		}
	}
}

func TestScanMigratesLineageFromCurrentWholeTaxonomyRecipe(t *testing.T) {
	c, db, write := identityScanFixture(t)
	taxonomyContent := `version: 1
kind: [tool]
subject:
  topics: [go, testing]
function: [security]
`
	write("taxonomy.yaml", taxonomyContent)
	write("tools/x.md", "---\ntags: [tool]\n---\n# X\n")
	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}

	doc := scannerDoc(t, db, "tools/x.md")
	taxonomyHash := fmt.Sprintf("%x", sha256.Sum256([]byte(taxonomyContent)))
	legacyAnalysis, legacyEnrichment, legacyExpand, legacyFreshness := legacyTaxonomyInputs(c, doc, taxonomyHash)
	if _, err := db.Exec(`UPDATE documents SET analysis_input_sha256=?,enrichment_input_sha256=?,
expand_input_sha256=?,freshness_input_sha256=?,analyzed_sha256=?,external_sha256=?,enriched_sha256=?,
expand_sha256=?,freshness_sha256=?
WHERE path=?`,
		legacyAnalysis, legacyEnrichment, legacyExpand, legacyFreshness,
		legacyAnalysis, legacyAnalysis, legacyEnrichment, legacyExpand, legacyFreshness, doc.Path); err != nil {
		t.Fatal(err)
	}

	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	got := scannerDoc(t, db, "tools/x.md")
	if got.AnalysisInputSHA256 == legacyAnalysis || got.AnalyzedSHA256 != got.AnalysisInputSHA256 {
		t.Fatalf("analysis lineage was not migrated: input=%q stamp=%q", got.AnalysisInputSHA256, got.AnalyzedSHA256)
	}
	if got.ExternalSHA256 != got.AnalysisInputSHA256 ||
		got.EnrichedSHA256 != got.EnrichmentInputSHA256 ||
		got.ExpandSHA256 != got.ExpandInputSHA256 ||
		got.FreshnessSHA256 != got.FreshnessInputSHA256 {
		t.Fatalf("task lineages were not migrated: %+v", got)
	}
}

func TestScanDoesNotMigrateLineageAcrossTaxonomyEditWithoutProof(t *testing.T) {
	c, db, write := identityScanFixture(t)
	oldTaxonomy := `version: 1
kind: [tool]
subject:
  topics: [go, testing]
function: [security]
`
	currentTaxonomy := `version: 1
kind: [tool]
subject:
  topics: [go, testing, unrelated]
function: [security]
`
	write("taxonomy.yaml", oldTaxonomy)
	write("tools/x.md", "---\ntags: [tool]\n---\n# X\n")
	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	doc := scannerDoc(t, db, "tools/x.md")

	oldTaxonomyHash := fmt.Sprintf("%x", sha256.Sum256([]byte(oldTaxonomy)))
	priorAnalysis, _, _, priorFreshness := legacyTaxonomyInputs(c, doc, oldTaxonomyHash)
	currentTaxonomyHash := fmt.Sprintf("%x", sha256.Sum256([]byte(currentTaxonomy)))
	currentLegacyAnalysis, currentLegacyEnrichment, currentLegacyExpand, currentLegacyFreshness := legacyTaxonomyInputs(c, doc, currentTaxonomyHash)
	// Simulate the previous scanner having updated input fingerprints after
	// the taxonomy edit, while its analysis job has not yet restamped this row.
	if _, err := db.Exec(`UPDATE documents SET analysis_input_sha256=?,enrichment_input_sha256=?,
expand_input_sha256=?,freshness_input_sha256=?,analyzed_sha256=?,external_sha256=?,enriched_sha256=?,
expand_sha256=?,freshness_sha256=?
WHERE path=?`,
		currentLegacyAnalysis, currentLegacyEnrichment, currentLegacyExpand,
		currentLegacyFreshness, priorAnalysis, priorAnalysis, priorAnalysis, priorAnalysis,
		priorFreshness, doc.Path); err != nil {
		t.Fatal(err)
	}
	write("taxonomy.yaml", currentTaxonomy)

	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	got := scannerDoc(t, db, "tools/x.md")
	if got.AnalyzedSHA256 != priorAnalysis || got.AnalysisInputSHA256 == got.AnalyzedSHA256 {
		t.Fatalf("unproven old lineage was restamped: input=%q stamp=%q", got.AnalysisInputSHA256, got.AnalyzedSHA256)
	}
}

func TestStubThresholdOnlyInvalidatesDocumentsThatChangeClassification(t *testing.T) {
	c, db, write := identityScanFixture(t)
	c.StubMaxBodyLines = 2
	write("short.md", "# Short\none\n")
	write("boundary.md", "# Boundary\none\ntwo\n")
	write("long.md", "# Long\none\ntwo\nthree\n")
	write("explicit.md", "---\ntags: [stub]\n---\n# Explicit\none\ntwo\nthree\n")
	write("private-boundary.md", "---\nkb_policy: {no_llm_context: true}\n---\n# Private\none\ntwo\n")
	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"short.md", "boundary.md", "long.md", "explicit.md", "private-boundary.md"} {
		doc := scannerDoc(t, db, path)
		if err := db.SaveAnalysis(t.Context(), doc, "analysis", "", "", nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := db.SaveEnrichment(t.Context(), doc, "fast", "", "", nil, nil); err != nil {
			t.Fatal(err)
		}
		if err := db.SaveTask(t.Context(), doc, config.TaskExpand, "expand", nil); err != nil {
			t.Fatal(err)
		}
		if err := db.SaveTask(t.Context(), doc, config.TaskFreshness, "freshness", nil); err != nil {
			t.Fatal(err)
		}
		if err := db.SaveEmbedding(t.Context(), doc, "embed", []byte{0, 0, 128, 63}); err != nil {
			t.Fatal(err)
		}
	}

	c.StubMaxBodyLines = 3
	if scan, err := Scan(t.Context(), c, db); err != nil || scan.Changed != 0 {
		t.Fatalf("stub-policy scan=%+v err=%v", scan, err)
	}
	for _, task := range []string{config.TaskAnalyze, config.TaskEnrich, config.TaskExpand, config.TaskFreshness} {
		var pending []database.Document
		var err error
		switch task {
		case config.TaskAnalyze:
			pending, err = db.Pending(t.Context(), "analysis", "fast", false)
		case config.TaskEnrich:
			pending, err = db.PendingEnrich(t.Context(), "fast", false)
		default:
			pending, err = db.PendingTask(t.Context(), task, task+"-model", false)
		}
		if err != nil {
			t.Fatalf("%s pending query: %v", task, err)
		}
		if len(pending) != 1 || pending[0].Path != "boundary.md" {
			t.Fatalf("%s pending=%v, want only boundary.md", task, pathsOf(pending))
		}
	}
	if pending, err := db.PendingTask(t.Context(), config.TaskEmbed, "embed", false); err != nil || len(pending) != 0 {
		t.Fatalf("stub threshold change invalidated embeddings: %v %v", pathsOf(pending), err)
	}
}

func TestTemplateChangeOnlyQueuesStubExpansion(t *testing.T) {
	c, db, write := identityScanFixture(t)
	c.StubMaxBodyLines = 2
	write("notes/STUB.md", "# Template v1\n")
	write("notes/example.md", "# Stub\none\n")
	write("notes/regular.md", "# Regular\none\ntwo\nthree\n")
	write("notes/private.md", "---\nkb_policy: {no_llm_context: true}\n---\n# Private\n")
	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"notes/example.md", "notes/regular.md", "notes/private.md"} {
		if err := db.SaveTask(t.Context(), scannerDoc(t, db, path), config.TaskExpand, "expand", nil); err != nil {
			t.Fatal(err)
		}
	}
	write("notes/STUB.md", "# Template v2\n")
	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	pending, err := db.PendingTask(t.Context(), config.TaskExpand, "expand", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Path != "notes/example.md" {
		t.Fatalf("expand pending=%v, want only notes/example.md", pathsOf(pending))
	}
}

func TestScanRejectsMissingRequiredPromptWithoutUpdatingDocuments(t *testing.T) {
	c, db, write := identityScanFixture(t)
	c.PromptDirectory = t.TempDir()
	c.MCPServer = config.MCPServerKnowledgeBase
	for name, body := range map[string]string{
		"base.md":                      "base",
		"knowledge-base-mcp-server.md": "mcp",
		"enrich.md":                    "enrich",
		"expand.md":                    "expand",
		"freshness.md":                 "freshness",
	} {
		if err := os.WriteFile(filepath.Join(c.PromptDirectory, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.md", "# A\n\nBody.\n")
	if _, err := Scan(t.Context(), c, db); err == nil || !strings.Contains(err.Error(), "analyze.md") {
		t.Fatalf("missing required prompt did not fail scan: %v", err)
	}
	docs, err := db.Documents(t.Context())
	if err != nil || len(docs) != 0 {
		t.Fatalf("scan partially updated docs on prompt error: docs=%v err=%v", pathsOf(docs), err)
	}
}

func TestMissingOptionalTaskPromptOnlyInvalidatesThatTaskWhenAdded(t *testing.T) {
	c, db, write := identityScanFixture(t)
	c.PromptDirectory = t.TempDir()
	c.MCPServer = config.MCPServerKnowledgeBase
	for name, body := range map[string]string{
		"base.md":                      "base",
		"knowledge-base-mcp-server.md": "mcp",
		"analyze.md":                   "analyze",
		"enrich.md":                    "enrich",
		"freshness.md":                 "freshness",
	} {
		if err := os.WriteFile(filepath.Join(c.PromptDirectory, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.md", "# A\n\nOne.\n\nTwo.\n\nThree.\n\nFour.\n\nFive.\n\nSix.\n")
	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	doc := scannerDoc(t, db, "a.md")
	if err := db.SaveAnalysis(t.Context(), doc, "analysis", "", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveEnrichment(t.Context(), doc, "fast", "", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveTask(t.Context(), doc, config.TaskExpand, "expand", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveTask(t.Context(), doc, config.TaskFreshness, "freshness", nil); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(c.PromptDirectory, "expand.md"), []byte("expand"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	if pending, err := db.Pending(t.Context(), "analysis", "fast", false); err != nil || len(pending) != 0 {
		t.Fatalf("adding expand prompt invalidated analysis: %v %v", pending, err)
	}
	if pending, err := db.PendingEnrich(t.Context(), "fast", false); err != nil || len(pending) != 0 {
		t.Fatalf("adding expand prompt invalidated enrichment: %v %v", pending, err)
	}
	if pending, err := db.PendingTask(t.Context(), config.TaskFreshness, "freshness", false); err != nil || len(pending) != 0 {
		t.Fatalf("adding expand prompt invalidated freshness: %v %v", pending, err)
	}
	if pending, err := db.PendingTask(t.Context(), config.TaskExpand, "expand", false); err != nil || len(pending) != 1 {
		t.Fatalf("expand prompt did not invalidate its task: %v %v", pending, err)
	}
}

func TestScanDoesNotRequireOptInFreshnessVerifyPrompt(t *testing.T) {
	c, db, write := identityScanFixture(t)
	c.PromptDirectory = t.TempDir()
	c.MCPServer = config.MCPServerKnowledgeBase
	for name, body := range map[string]string{
		"base.md":                      "base",
		"knowledge-base-mcp-server.md": "mcp",
		"analyze.md":                   "analyze",
		"enrich.md":                    "enrich",
		"expand.md":                    "expand",
		"freshness.md":                 "freshness",
	} {
		if err := os.WriteFile(filepath.Join(c.PromptDirectory, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.md", "# A\n\nBody.\n")
	if _, err := Scan(t.Context(), c, db); err != nil {
		t.Fatalf("scan should not require the opt-in verifier prompt: %v", err)
	}
}

func legacyTaxonomyInputs(c config.Config, doc database.Document, taxonomySHA string) (analysis, enrichment, expand, freshness string) {
	stubLines := fmt.Sprintf("stub_max_body_lines=%d", c.StubLineLimit())
	analysis = hashInputs(doc.SemanticSHA256, "", taxonomySHA,
		"write_scope="+c.EffectiveWriteScope(), stubLines)
	enrichment = hashInputs(doc.SemanticSHA256, "", taxonomySHA,
		"enrichment_mode="+string(c.EnrichmentMode), stubLines)
	expand = hashInputs(doc.SemanticSHA256, "", "", taxonomySHA, stubLines)
	freshness = hashInputs(doc.SemanticSHA256, "", stubLines)
	return analysis, enrichment, expand, freshness
}

func pathsOf(docs []database.Document) []string {
	paths := make([]string, 0, len(docs))
	for _, doc := range docs {
		paths = append(paths, doc.Path)
	}
	return paths
}
