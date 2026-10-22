package analyzer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/scanner"
)

type orderedModel struct {
	stage     string
	events    *[]string
	prompts   []string
	unloadErr error
}

func (m *orderedModel) Generate(_ context.Context, prompt string, _ json.RawMessage) (string, error) {
	*m.events = append(*m.events, m.stage)
	m.prompts = append(m.prompts, prompt)
	if m.stage == "enrich" {
		return `{"title":"Enriched title","summary":"fast","tags":[],"frontmatter_patch":{"set":{},"remove":[]}}`, nil
	}
	return `{"title":"Analysis","summary":"full","tags":[],"obsolete":false,"suggestions":[]}`, nil
}

func (m *orderedModel) Unload(context.Context) error {
	*m.events = append(*m.events, "unload "+m.stage)
	return m.unloadErr
}

type orderedEmbedder struct {
	events *[]string
}

func (m *orderedEmbedder) Embed(context.Context, string) ([]float32, error) {
	*m.events = append(*m.events, "embed")
	return []float32{1, 0}, nil
}

func (m *orderedEmbedder) Unload(context.Context) error {
	*m.events = append(*m.events, "unload embed")
	return nil
}

type generatorFunc func(context.Context, string, json.RawMessage) (string, error)

func (f generatorFunc) Generate(ctx context.Context, prompt string, format json.RawMessage) (string, error) {
	return f(ctx, prompt, format)
}

func TestPipelineBatchesAndUnloadsModelsBetweenStages(t *testing.T) {
	c, db := setup(t)
	c.OllamaModelFast = "fast-model"
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "b.md"), []byte("# Another note"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	c.OllamaEmbedModel = "test-embedder"
	var events []string
	fast := &orderedModel{stage: "enrich", events: &events}
	full := &orderedModel{stage: "analysis", events: &events}
	result, err := RunProgress(t.Context(), c, db, Generators{
		Analysis: full, Enrich: fast, Embed: &orderedEmbedder{events: &events},
	}, RunOptions{})
	if err != nil || result.Enriched != 2 || result.Processed != 2 {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	want := []string{"embed", "embed", "unload embed", "enrich", "enrich", "unload enrich", "analysis", "analysis"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events=%v, want %v", events, want)
	}
	for _, prompt := range full.prompts {
		if !strings.Contains(prompt, `"Enriched title"`) {
			t.Fatalf("candidate enrichment missing from analysis prompt:\n%s", prompt)
		}
	}
}

func TestEnrichmentFailureFallsThroughAndRetriesOnlyEnrichment(t *testing.T) {
	c, db := setup(t)
	c.OllamaModelFast = "fast-model"
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "b.md"), []byte("# Another note"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	calls := 0
	fast := generatorFunc(func(_ context.Context, prompt string, _ json.RawMessage) (string, error) {
		calls++
		if calls == 1 && strings.Contains(prompt, "Current path: a.md\n") {
			return "", errors.New("model failure")
		}
		return `{"title":"Fast","summary":"Fast summary","tags":[]}`, nil
	})
	full := &fakeLLM{reply: `{"title":"B","summary":"S","tags":[],"suggestions":[]}`}
	result, err := RunProgress(t.Context(), c, db, Generators{Analysis: full, Enrich: fast}, RunOptions{})
	if err != nil || calls != 2 || len(full.prompts) != 2 || result.Failed != 0 || result.EnrichmentFailed != 1 || result.Processed != 2 {
		t.Fatalf("result=%+v enrich calls=%d analysis calls=%d error=%v", result, calls, len(full.prompts), err)
	}
	docs, err := db.Documents(t.Context())
	if err != nil || docs[0].EnrichedSHA256 != "" || docs[0].AnalyzedSHA256 != docs[0].SHA256 || docs[0].LastError != "" {
		t.Fatalf("fallback state=%+v error=%v", docs, err)
	}
	var message string
	if err := db.QueryRow(`SELECT error FROM stage_results WHERE document_path='a.md' AND stage='enrich'`).Scan(&message); err != nil || message != "model failure" {
		t.Fatalf("durable enrichment error=%q error=%v", message, err)
	}
	result, err = RunProgress(t.Context(), c, db, Generators{Analysis: full, Enrich: fast}, RunOptions{})
	if err != nil || calls != 3 || len(full.prompts) != 2 || result.Enriched != 1 || result.Processed != 0 || result.EnrichmentFailed != 0 {
		t.Fatalf("retry=%+v enrich calls=%d analysis calls=%d error=%v", result, calls, len(full.prompts), err)
	}
	pending, err := db.Pending(t.Context(), c.OllamaModel, c.FastModel(), false)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after retry=%+v error=%v", pending, err)
	}
	docs, err = db.Documents(t.Context())
	if err != nil || docs[0].Title != "B" || docs[0].Summary != "S" {
		t.Fatalf("late enrichment overwrote successful analysis: %+v error=%v", docs, err)
	}
}

func TestCancellationStopsBeforeNextDocumentOrModel(t *testing.T) {
	c, db := setup(t)
	c.OllamaModelFast = "fast-model"
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "b.md"), []byte("# Another note"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fast := &fakeLLM{reply: `{"title":"A","summary":"S","tags":[]}`}
	full := &fakeLLM{}
	result, err := RunProgress(ctx, c, db, Generators{Analysis: full, Enrich: fast}, RunOptions{
		Progress: func(p Progress) {
			if p.Outcome == "enriched" {
				cancel()
			}
		},
	})
	if !errors.Is(err, context.Canceled) || len(fast.prompts) != 1 || len(full.prompts) != 0 || result.Enriched != 1 {
		t.Fatalf("result=%+v enrich=%d full=%d error=%v", result, len(fast.prompts), len(full.prompts), err)
	}
}

func TestLivePrivacyPolicyIsCheckedBeforeEmbeddingAndGeneration(t *testing.T) {
	c, db := setup(t)
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	content := "---\nkb_policy:\n  no_llm_context: true\n---\nSecret added after scan.\n"
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "a.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	embed := &fakeEmbedder{}
	full := &fakeLLM{}
	result, err := RunProgress(t.Context(), c, db, Generators{Analysis: full, Embed: embed}, RunOptions{})
	if err != nil || result.Skipped != 1 || len(embed.texts) != 0 || len(full.prompts) != 0 {
		t.Fatalf("result=%+v embeds=%d prompts=%d error=%v", result, len(embed.texts), len(full.prompts), err)
	}
}

func TestPrivacyChangedDuringEnrichmentIsOmittedFromAnalysisCandidates(t *testing.T) {
	c, db := setup(t)
	c.OllamaModelFast = "fast-model"
	path := filepath.Join(c.RepositoryPath, "b.md")
	if err := os.WriteFile(path, []byte("# Public before enrichment"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	fast := generatorFunc(func(context.Context, string, json.RawMessage) (string, error) {
		private := "---\nkb_policy:\n  no_llm_context: true\n---\nNow private.\n"
		if err := os.WriteFile(path, []byte(private), 0644); err != nil {
			return "", err
		}
		return `{"title":"A","summary":"S","tags":[]}`, nil
	})
	full := &fakeLLM{reply: `{"title":"A","summary":"S","tags":[],"suggestions":[]}`}
	result, err := RunProgress(t.Context(), c, db, Generators{Analysis: full, Enrich: fast}, RunOptions{})
	if err != nil || result.Skipped != 1 || result.Processed != 1 || len(full.prompts) != 1 {
		t.Fatalf("result=%+v prompts=%d error=%v", result, len(full.prompts), err)
	}
	if strings.Contains(full.prompts[0], "b.md") || strings.Contains(full.prompts[0], "Now private") {
		t.Fatalf("private candidate leaked:\n%s", full.prompts[0])
	}
}

func TestUnloadFailureDoesNotStartNextModel(t *testing.T) {
	c, db := setup(t)
	c.OllamaModelFast = "fast-model"
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	var events []string
	fast := &orderedModel{stage: "enrich", events: &events, unloadErr: errors.New("release failed")}
	full := &orderedModel{stage: "analysis", events: &events}
	_, err := RunProgress(t.Context(), c, db, Generators{Analysis: full, Enrich: fast}, RunOptions{})
	if err == nil || !strings.Contains(err.Error(), "release failed") || len(full.prompts) != 0 {
		t.Fatalf("events=%v error=%v", events, err)
	}
}
