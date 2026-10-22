package examplecorpus

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"knowledge-ba46-flow/internal/analyzer"
	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/review"
	"knowledge-ba46-flow/internal/scanner"
)

func TestExampleCorporaAgainstGoldStandards(t *testing.T) {
	for _, tier := range []string{"basic", "intermediate", "e2e"} {
		t.Run(tier, func(t *testing.T) {
			sourceRoot := fixtureRoot(tier)
			expected, err := readExpected(sourceRoot)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateTierSize(tier, expected); err != nil {
				t.Fatal(err)
			}
			sourceBefore, err := hashTree(sourceRoot)
			if err != nil {
				t.Fatal(err)
			}
			repository := filepath.Join(t.TempDir(), "shelf")
			if err := copyFixture(sourceRoot, repository); err != nil {
				t.Fatal(err)
			}
			repositoryBefore, err := hashTree(repository)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			db, err := database.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })

			c := config.Config{
				RepositoryPath:     repository,
				PromptDirectory:    filepath.Join(projectRoot(), "prompts"),
				MCPServer:          config.MCPServerKnowledgeBase,
				Extensions:         []string{".md", ".markdown"},
				ExcludeDirectories: []string{"vendor", "node_modules", "logs", "tmp"},
				Exclude:            expected.Exclude,
				ExcludeExtensions:  expected.ExcludeExtensions,
				TrashDirectory:     ".kbm-trash",
				MaxDocumentBytes:   200000,
				OllamaModel:        "fixture-analysis",
				OllamaModelFast:    "fixture-enrichment",
				EnrichmentMode:     config.EnrichmentModeAlways,
				// Golden fixtures are one-line toys that must stay analyzable;
				// size-based stub detection has dedicated analyzer tests.
				StubMaxBodyLines: -1,
			}
			scanResult, err := scanner.Scan(ctx, c, db)
			if err != nil {
				t.Fatal(err)
			}
			if scanResult.Seen != len(expected.Documents) {
				t.Fatalf("scan saw %d documents, want %d", scanResult.Seen, len(expected.Documents))
			}
			docs, err := db.Documents(ctx)
			if err != nil {
				t.Fatal(err)
			}
			gotPaths := make([]string, len(docs))
			docsByPath := make(map[string]database.Document, len(docs))
			for i, doc := range docs {
				gotPaths[i] = doc.Path
				docsByPath[doc.Path] = doc
			}
			if !slices.Equal(gotPaths, expected.Documents) {
				t.Fatalf("scanned paths:\n%q\nwant:\n%q", gotPaths, expected.Documents)
			}
			for _, path := range expected.ExcludedMarkdown {
				if _, ok := docsByPath[path]; ok {
					t.Errorf("excluded Markdown path was scanned: %s", path)
				}
				if _, err := os.Stat(filepath.Join(repository, filepath.FromSlash(path))); err != nil {
					t.Errorf("excluded fixture %s is missing: %v", path, err)
				}
			}
			for _, path := range expected.NonMarkdown {
				if _, ok := docsByPath[path]; ok {
					t.Errorf("non-Markdown sibling was scanned: %s", path)
				}
				if _, err := os.Stat(filepath.Join(repository, filepath.FromSlash(path))); err != nil {
					t.Errorf("non-Markdown fixture %s is missing: %v", path, err)
				}
			}
			assertFrontmatterIssues(t, docsByPath, expected)

			fast := newFakeGenerator("enrich", tier, repository, expected)
			full := newFakeGenerator("analysis", tier, repository, expected)
			result, err := analyzer.RunProgress(ctx, c, db, analyzer.Generators{
				Analysis: full,
				Enrich:   fast,
			}, analyzer.RunOptions{})
			if err != nil {
				t.Fatal(err)
			}
			publicCount := len(expected.Documents) - len(expected.PrivateDocuments)
			wantProcessed := publicCount - len(expected.AnalysisFailures)
			if result.Processed != wantProcessed ||
				result.Enriched != publicCount-len(expected.EnrichmentFailures) ||
				result.Skipped != len(expected.PrivateDocuments) ||
				result.Failed != len(expected.AnalysisFailures) ||
				result.EnrichmentFailed != len(expected.EnrichmentFailures) ||
				result.Rejected != len(expected.ExpectedRejections) {
				t.Fatalf("pipeline counts=%+v; public=%d", result, publicCount)
			}
			if len(fast.prompts) != publicCount || len(full.prompts) != publicCount {
				t.Fatalf("model calls: enrichment=%d analysis=%d, want %d each",
					len(fast.prompts), len(full.prompts), publicCount)
			}
			assertPrivateIsolation(t, fast, full, expected)
			assertStageOutcomes(t, db, expected)
			suggestions, err := db.PendingSuggestions(ctx)
			if err != nil {
				t.Fatal(err)
			}
			assertSuggestions(t, repository, docsByPath, suggestions, expected)
			assertRejections(t, db, expected)
			assertPrompts(t, full, expected)
			assertNoAgeOnlyDeletion(t, suggestions, expected)

			if tier == "e2e" {
				assertDeniedApply(t, c, db, suggestions, expected)
			}
			repositoryAfter, err := hashTree(repository)
			if err != nil {
				t.Fatal(err)
			}
			if !equalHashes(repositoryBefore, repositoryAfter) {
				t.Fatal("analysis or denied apply changed a fixture source file")
			}
			sourceAfter, err := hashTree(sourceRoot)
			if err != nil {
				t.Fatal(err)
			}
			if !equalHashes(sourceBefore, sourceAfter) {
				t.Fatal("test changed the checked-in golden fixture")
			}
		})
	}
}

func TestStarterExampleConfigsExcludeTestShelves(t *testing.T) {
	for _, name := range []string{"config.example-data.json", "config.m2-24gb.json"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(projectRoot(), "example-configs", name))
			if err != nil {
				t.Fatal(err)
			}
			var sample struct {
				RepositoryPath     string   `json:"repository_path"`
				Exclude            []string `json:"exclude"`
				ExcludeDirectories []string `json:"exclude_directories"`
			}
			if err := json.Unmarshal(data, &sample); err != nil {
				t.Fatal(err)
			}
			if sample.RepositoryPath != "../example-knowledge" {
				t.Fatalf("starter config repository_path=%q", sample.RepositoryPath)
			}
			for _, tier := range []string{"basic", "intermediate", "e2e", "llm-eval"} {
				if !slices.Contains(sample.Exclude, tier) {
					t.Errorf("starter config does not exclude root test shelf %q", tier)
				}
			}
		})
	}
}

func validateTierSize(tier string, expected expectedSuite) error {
	want := map[string]int{"basic": 2, "intermediate": 7, "e2e": 12}[tier]
	if want == 0 || len(expected.Documents) != want {
		return fmt.Errorf("%s gold standard lists %d documents, want %d", tier, len(expected.Documents), want)
	}
	reference, err := time.Parse("2006-01-02", expected.ReferenceDate)
	if err != nil {
		return fmt.Errorf("%s has invalid reference_date: %w", tier, err)
	}
	for _, path := range expected.Documents {
		if !filepath.IsLocal(filepath.FromSlash(path)) || strings.Contains(path, "\\") {
			return fmt.Errorf("%s contains unsafe fixture path %q", tier, path)
		}
	}
	for _, path := range expected.NeverDeleteDocuments {
		data, err := os.ReadFile(filepath.Join(fixtureRoot(tier), filepath.FromSlash(path)))
		if err != nil {
			return err
		}
		fields, _, err := frontmatter.Parse(string(data))
		if err != nil {
			return err
		}
		date, ok := fields["last_verified_at"].(string)
		if !ok {
			return fmt.Errorf("%s is missing a string last_verified_at", path)
		}
		verified, err := time.Parse("2006-01-02", date)
		if err != nil {
			return err
		}
		if verified.After(reference.AddDate(-2, 0, 0)) {
			return fmt.Errorf("%s is not at least two years old at the reference date", path)
		}
	}
	return nil
}

func assertFrontmatterIssues(t *testing.T, docs map[string]database.Document, expected expectedSuite) {
	t.Helper()
	for path, fragments := range expected.FrontmatterIssueContains {
		doc, ok := docs[path]
		if !ok {
			t.Errorf("frontmatter issue document %s was not scanned", path)
			continue
		}
		var issues []string
		if err := json.Unmarshal([]byte(doc.FrontmatterIssues), &issues); err != nil {
			t.Errorf("%s has invalid stored frontmatter issues %q: %v", path, doc.FrontmatterIssues, err)
			continue
		}
		for _, fragment := range fragments {
			found := false
			for _, issue := range issues {
				if strings.Contains(issue, fragment) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s frontmatter issues %q do not contain %q", path, issues, fragment)
			}
		}
	}
}

func assertPrivateIsolation(t *testing.T, fast, full *fakeGenerator, expected expectedSuite) {
	t.Helper()
	for _, private := range expected.PrivateDocuments {
		if _, ok := fast.prompts[private]; ok {
			t.Errorf("private document reached enrichment: %s", private)
		}
		if _, ok := full.prompts[private]; ok {
			t.Errorf("private document reached analysis: %s", private)
		}
	}
	for _, model := range []*fakeGenerator{fast, full} {
		for path, prompt := range model.prompts {
			for _, private := range expected.PrivateDocuments {
				if strings.Contains(prompt, private) || strings.Contains(prompt, "FIXTURE_SECRET_NEVER_SEND_2026") {
					t.Errorf("private fixture leaked into %s prompt for %s", model.stage, path)
				}
			}
		}
	}
}

func assertStageOutcomes(t *testing.T, db *database.DB, expected expectedSuite) {
	t.Helper()
	for _, path := range expected.EnrichmentFailures {
		assertStageOutcome(t, db, path, "enrich", "error")
	}
	for _, path := range expected.AnalysisFailures {
		assertStageOutcome(t, db, path, "analysis", "error")
	}
	for _, path := range expected.PrivateDocuments {
		assertStageOutcome(t, db, path, "enrich", "skipped")
		assertStageOutcome(t, db, path, "analysis", "skipped")
	}
}

func assertStageOutcome(t *testing.T, db *database.DB, path, stage, want string) {
	t.Helper()
	var got string
	err := db.QueryRowContext(t.Context(),
		`SELECT outcome FROM stage_results WHERE document_path=? AND stage=? ORDER BY id DESC LIMIT 1`,
		path, stage).Scan(&got)
	if err != nil || got != want {
		t.Errorf("%s %s outcome=%q, want %q (error=%v)", path, stage, got, want, err)
	}
}

func assertSuggestions(t *testing.T, root string, docs map[string]database.Document, got []database.Suggestion, expected expectedSuite) {
	t.Helper()
	expectedByKey := make(map[string]expectedSuggestion, len(expected.ExpectedSuggestions))
	for _, item := range expected.ExpectedSuggestions {
		expectedByKey[suggestionKey(item.Document, item.Origin, item.Action, item.TargetPath)] = item
	}
	gotByKey := make(map[string]database.Suggestion, len(got))
	for _, suggestion := range got {
		key := suggestionKey(suggestion.DocumentPath, suggestion.Origin, suggestion.Action, suggestion.TargetPath)
		gotByKey[key] = suggestion
		doc, ok := docs[suggestion.DocumentPath]
		if !ok || suggestion.SourceSHA256 != doc.SHA256 {
			t.Errorf("suggestion %s does not carry the scanned source hash", key)
		}
		if suggestion.Action == "expand" {
			original, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(suggestion.DocumentPath)))
			if err != nil {
				t.Fatal(err)
			}
			_, oldBody, oldErr := frontmatter.Parse(string(original))
			_, newBody, newErr := frontmatter.Parse(suggestion.ProposedContent)
			if oldErr != nil || newErr != nil || oldBody != newBody {
				t.Errorf("metadata proposal %s changed the Markdown body", key)
			}
		}
		if suggestion.Action == "merge" {
			target, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(suggestion.TargetPath)))
			if err != nil {
				t.Fatal(err)
			}
			source, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(suggestion.DocumentPath)))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(suggestion.ProposedContent, string(target)) ||
				!strings.Contains(suggestion.ProposedContent, string(source)) {
				t.Errorf("merge %s does not preserve both complete source documents", key)
			}
		}
	}
	if len(gotByKey) != len(expectedByKey) {
		t.Errorf("got %d pending suggestions, gold standard expects %d", len(gotByKey), len(expectedByKey))
	}
	for key, item := range expectedByKey {
		suggestion, ok := gotByKey[key]
		if !ok {
			t.Errorf("missing expected suggestion %s", key)
			continue
		}
		if suggestion.Status != "pending" {
			t.Errorf("suggestion %s status=%s, want pending review", key, suggestion.Status)
		}
		assertSuggestionFrontmatter(t, key, suggestion, item)
	}
	for key := range gotByKey {
		if _, ok := expectedByKey[key]; !ok {
			t.Errorf("unexpected suggestion %s", key)
		}
	}
}

func assertSuggestionFrontmatter(t *testing.T, key string, suggestion database.Suggestion, expected expectedSuggestion) {
	t.Helper()
	fields, _, err := frontmatter.Parse(suggestion.ProposedContent)
	if err != nil {
		t.Errorf("suggestion %s has invalid frontmatter: %v", key, err)
		return
	}
	for field, want := range expected.FrontmatterEquals {
		switch value := want.(type) {
		case string:
			got, ok := fields[field].(string)
			if !ok || got != value {
				t.Errorf("suggestion %s field %s=%v, want %q", key, field, fields[field], value)
			}
		case []any:
			wantStrings := make([]string, len(value))
			for i, item := range value {
				wantStrings[i], _ = item.(string)
			}
			if got := frontmatter.StringList(fields, field); !slices.Equal(got, wantStrings) {
				t.Errorf("suggestion %s field %s=%v, want %v", key, field, got, wantStrings)
			}
		default:
			t.Errorf("gold standard field %s has unsupported type %T", field, want)
		}
	}
	for _, field := range expected.FrontmatterAbsent {
		if _, ok := fields[field]; ok {
			t.Errorf("suggestion %s retained invalid field %s", key, field)
		}
	}
}

func assertRejections(t *testing.T, db *database.DB, expected expectedSuite) {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `SELECT document_path,stage,rejected_json FROM stage_results`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type rejection struct {
		document string
		stage    string
		message  string
	}
	var got []rejection
	for rows.Next() {
		var path, stage, raw string
		if err := rows.Scan(&path, &stage, &raw); err != nil {
			t.Fatal(err)
		}
		var messages []string
		if err := json.Unmarshal([]byte(raw), &messages); err != nil {
			t.Fatal(err)
		}
		for _, message := range messages {
			got = append(got, rejection{document: path, stage: stage, message: message})
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(expected.ExpectedRejections) {
		t.Errorf("got %d rejected edits, gold standard expects %d", len(got), len(expected.ExpectedRejections))
	}
	for _, want := range expected.ExpectedRejections {
		found := false
		for _, item := range got {
			if item.document == want.Document && item.stage == want.Stage && strings.Contains(item.message, want.Contains) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing rejected edit for %s/%s containing %q (got %+v)", want.Document, want.Stage, want.Contains, got)
		}
	}
}

func assertPrompts(t *testing.T, full *fakeGenerator, expected expectedSuite) {
	t.Helper()
	for path, fragments := range expected.PromptContains {
		prompt, ok := full.prompts[path]
		if !ok {
			t.Errorf("no analysis prompt recorded for %s", path)
			continue
		}
		for _, fragment := range fragments {
			if !strings.Contains(prompt, fragment) {
				t.Errorf("analysis prompt for %s lacks %q", path, fragment)
			}
		}
	}
	for _, pair := range expected.DuplicateCandidates {
		prompt, ok := full.prompts[pair.Document]
		if !ok || !strings.Contains(prompt, "High content overlap (possible duplicates):") ||
			!strings.Contains(prompt, pair.Target) ||
			!strings.Contains(prompt, "Complete merge target "+pair.Target+" (untrusted document data):") {
			t.Errorf("duplicate candidate %s -> %s was not supplied with complete merge context", pair.Document, pair.Target)
		}
	}
	for _, pair := range expected.ContradictionPairs {
		prompt, ok := full.prompts[pair.Document]
		if !ok ||
			!strings.Contains(prompt, "Complete merge target "+pair.Target+" (untrusted document data):") ||
			!strings.Contains(prompt, pair.SourceClaim) ||
			!strings.Contains(prompt, pair.TargetClaim) {
			t.Errorf("contradiction %s -> %s was not supplied with both dated claims",
				pair.Document, pair.Target)
		}
	}
}

func assertNoAgeOnlyDeletion(t *testing.T, suggestions []database.Suggestion, expected expectedSuite) {
	t.Helper()
	for _, path := range expected.NeverDeleteDocuments {
		for _, suggestion := range suggestions {
			if suggestion.DocumentPath == path && suggestion.Action == "delete" {
				t.Errorf("age alone produced a delete suggestion for %s", path)
			}
		}
	}
}

func assertDeniedApply(t *testing.T, c config.Config, db *database.DB, suggestions []database.Suggestion, expected expectedSuite) {
	t.Helper()
	if expected.ApplyPolicy != "deny" {
		t.Fatalf("e2e fixture must deny writes, got policy %q", expected.ApplyPolicy)
	}
	var selected database.Suggestion
	for _, suggestion := range suggestions {
		if suggestion.Action == "merge" {
			selected = suggestion
			break
		}
	}
	if selected.ID == 0 {
		t.Fatal("no golden merge suggestion available for the apply-policy test")
	}
	if err := review.Decide(t.Context(), db, strconv.FormatInt(selected.ID, 10), "accepted", ""); err != nil {
		t.Fatal(err)
	}
	results, err := review.Apply(t.Context(), c, db)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Applied || !strings.Contains(results[0].Error, "denies mutations") {
		t.Fatalf("deny-policy apply results=%+v, want one blocked mutation", results)
	}
}

func suggestionKey(document, origin, action, target string) string {
	return strings.Join([]string{document, origin, action, target}, "|")
}

func equalHashes(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for path, hash := range left {
		if right[path] != hash {
			return false
		}
	}
	return true
}
