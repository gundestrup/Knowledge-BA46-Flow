package analyzer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/scanner"
	"knowledge-ba46-flow/internal/similarity"
)

type switchingEmbedder func(context.Context, string) ([]float32, error)

func (f switchingEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	return f(ctx, text)
}

func TestPipelineRefreshesEmbeddingModelWithoutRepeatingGeneration(t *testing.T) {
	c, db := setup(t)
	c.OllamaEmbedModel = "first-embedder"
	note := "---\ntags: [tool]\nkbm:\n  link_check:\n    at: old\n---\n# Visible\n\nBody text.\n"
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "b.md"), []byte(note), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "private.md"),
		[]byte("---\nkb_policy:\n  no_llm_context: true\n---\nSECRET PRIVATE BODY\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	gen := &fakeLLM{reply: `{"title":"Note","summary":"s","tags":[],"obsolete":false,"suggestions":[]}`}
	first := &fakeEmbedder{}
	if _, err := RunProgress(t.Context(), c, db, Generators{Analysis: gen, Embed: first}, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	generationCalls := len(gen.prompts)
	c.OllamaTaskModels = map[string][]string{config.TaskEmbed: {"second-embedder", "first-embedder"}}
	second := &fakeEmbedder{}
	result, err := RunProgress(t.Context(), c, db, Generators{Analysis: gen, Embed: second}, RunOptions{})
	if err != nil || result.Processed != 0 || len(gen.prompts) != generationCalls || len(second.texts) != 2 {
		t.Fatalf("embedding switch repeated generation or skipped vectors: result=%+v calls=%d embeds=%d error=%v",
			result, len(gen.prompts), len(second.texts), err)
	}
	for _, text := range append(first.texts, second.texts...) {
		if strings.Contains(text, "SECRET PRIVATE BODY") || strings.Contains(text, "link_check") {
			t.Fatalf("private/managed data leaked into embedding input: %s", text)
		}
	}
	docs, err := db.Documents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range docs {
		if doc.Path == "private.md" {
			if len(doc.Embedding) != 0 {
				t.Fatal("private document was embedded")
			}
			continue
		}
		if doc.EmbeddedModel != "second-embedder" || doc.EmbeddedSHA256 != doc.SemanticSHA256 {
			t.Fatalf("vector provenance is stale: %+v", doc)
		}
	}
	updated, err := frontmatter.SetManaged(note, "link_check", map[string]any{"at": "new"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "b.md"), []byte(updated), 0644); err != nil {
		t.Fatal(err)
	}
	if scan, err := scanner.Scan(t.Context(), c, db); err != nil || scan.Changed != 0 {
		t.Fatalf("managed metadata invalidated source hashes: %+v error=%v", scan, err)
	}
	if _, err := RunProgress(t.Context(), c, db, Generators{Analysis: gen, Embed: second}, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(second.texts) != 2 {
		t.Fatal("managed-only writes requeued embeddings")
	}
}

func TestPartialEmbeddingSwitchNeverComparesDifferentModels(t *testing.T) {
	c, db := setup(t)
	c.OllamaEmbedModel = "new-embedder"
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "b.md"), []byte("# Another note"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	docs, err := db.Documents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range docs {
		if err := db.SaveEmbedding(t.Context(), doc, "old-embedder", similarity.MarshalEmbedding([]float32{1, 0})); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	embed := switchingEmbedder(func(context.Context, string) ([]float32, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("embedding service unavailable")
		}
		return []float32{0, 1}, nil // Same dimensions, a different vector space.
	})
	p := &pipeline{
		ctx: t.Context(), c: c, db: db, embedModel: "new-embedder",
		gens: Generators{Embed: embed}, emit: func(Progress) {},
	}
	if err := p.loadCorpus(); err != nil {
		t.Fatal(err)
	}
	if len(p.embeds["a.md"]) != 2 || len(p.embeds["b.md"]) != 0 {
		t.Fatalf("incompatible cached vectors entered the same ranking: %+v", p.embeds)
	}
	if doc := docByPath(t, db, "b.md"); doc.EmbeddedModel != "old-embedder" {
		t.Fatalf("failed refresh stamped an old vector as the new model: %+v", doc)
	}
}

func TestStandaloneEmbedRefreshesModelDriftWithoutReanalyzeFlag(t *testing.T) {
	c, db := setup(t)
	c.OllamaEmbedModel = "first-embedder"
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	embed := &fakeEmbedder{}
	if _, err := RunEmbed(t.Context(), c, db, Generators{Embed: embed}, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	c.OllamaTaskModels = map[string][]string{config.TaskEmbed: {"first-embedder", "second-embedder"}}
	result, err := RunEmbed(t.Context(), c, db, Generators{Embed: embed}, RunOptions{EmbedModel: "second-embedder"})
	if err != nil || result.Processed != 1 || len(embed.texts) != 2 {
		t.Fatalf("explicit model drift did not refresh vectors: %+v calls=%d error=%v", result, len(embed.texts), err)
	}
	if doc := docByPath(t, db, "a.md"); doc.EmbeddedModel != "second-embedder" {
		t.Fatalf("wrong embedding lineage: %+v", doc)
	}
}
