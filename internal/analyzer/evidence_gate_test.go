package analyzer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/scanner"
	"knowledge-ba46-flow/internal/similarity"
)

// TestEmbeddingDependentsRespectCalibratedGate reproduces the dense-model
// incident: an embedding model whose cosine distribution sits above the old
// absolute 0.72 threshold flagged a quarter of all pairs, so one changed
// note dragged nearly the whole repository back through analysis. The gate
// must instead take each changed note's few nearest neighbors and only when
// they clear the corpus's own sampled high-percentile floor.
func TestEmbeddingDependentsRespectCalibratedGate(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	c.OllamaEmbedModel = "test-embedder"

	// changed.md anchors a 20-note cluster of near-identical vectors; twenty
	// unrelated notes each get their own dimension — the corpus pair
	// distribution is bimodal, so the calibrated floor sits above the noise.
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(c.RepositoryPath, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 20; i++ {
		write(fmt.Sprintf("near-%02d.md", i), fmt.Sprintf("# Near %d\n\n%s", i, strings.Repeat(fmt.Sprintf("nearword%d ", i), 12)))
		write(fmt.Sprintf("far-%02d.md", i), fmt.Sprintf("# Far %d\n\n%s", i, strings.Repeat(fmt.Sprintf("farword%d ", i+100), 12)))
	}
	write("changed.md", "# Changed\n\nanchor words here.")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	docs, err := db.Documents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Stamp vectors directly: cluster notes share the identical direction
	// (pairwise cosine exactly 1), far notes each own one basis axis (0) —
	// the bimodal distribution puts the calibrated floor inside the cluster.
	for i, d := range docs {
		v := make([]float32, 48)
		if strings.HasPrefix(d.Path, "near-") || d.Path == "changed.md" {
			v[0] = 1
		} else {
			v[4+i] = 1
		}
		if err := db.SaveEmbedding(ctx, d, "test-embedder", similarity.MarshalEmbedding(v)); err != nil {
			t.Fatal(err)
		}
	}
	reply := `{"title":"T","summary":"S","tags":[],"obsolete":false,"suggestions":[]}`
	first := &fakeLLM{reply: reply}
	if _, err := RunProgress(ctx, c, db, Generators{Analysis: first, Embed: &fakeEmbedder{}}, RunOptions{}); err != nil {
		t.Fatal(err)
	}

	// Editing the anchor makes it pending and its stored vector stale — the
	// dry-run estimate has no embedder, so it reports the pending queue only.
	write("changed.md", "# Changed\n\nanchor words revised.")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	est, err := EstimateAnalysis(ctx, c, db, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if est.Pending != 1 || est.Total != 1 {
		t.Fatalf("dry-run estimate=%+v, want pending=1 total=1", est)
	}

	// The real run backfills the anchor's vector into the cluster; the sweep
	// may then pull at most evidenceNeighborLimit nearest neighbors — never
	// the far notes, never a whole-corpus pass like the old absolute bar.
	emb := switchingEmbedder(func(context.Context, string) ([]float32, error) {
		v := make([]float32, 48)
		v[0] = 1
		return v, nil
	})
	second := &fakeLLM{reply: reply}
	result, err := RunProgress(ctx, c, db, Generators{Analysis: second, Embed: emb}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Processed != 1+evidenceNeighborLimit {
		t.Fatalf("processed=%d, want %d (anchor + capped neighbors)", result.Processed, 1+evidenceNeighborLimit)
	}
	for _, prompt := range second.prompts {
		if strings.Contains(prompt, "Current path: far-") {
			t.Fatalf("unrelated note was re-analyzed on embedding proximity alone: %s", prompt[:200])
		}
	}
}

// TestEvidenceGateSilentWithoutEmbeddings keeps the sweep purely lexical when
// no embedding model is configured — and a degenerate all-zero vector space
// must not fabricate dependencies either.
func TestEvidenceGateSilentWithoutEmbeddings(t *testing.T) {
	ctx := context.Background()
	c, db := setup(t)
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(c.RepositoryPath, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 8; i++ {
		write(fmt.Sprintf("peer-%02d.md", i), fmt.Sprintf("# Peer %d\n\n%s", i, strings.Repeat(fmt.Sprintf("peerword%d ", i), 10)))
	}
	write("changed.md", "# Changed\n\nanchor words here.")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	reply := `{"title":"T","summary":"S","tags":[],"obsolete":false,"suggestions":[]}`
	if _, err := RunProgress(ctx, c, db, Generators{Analysis: &fakeLLM{reply: reply}}, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	write("changed.md", "# Changed\n\nanchor words revised.")
	if _, err := scanner.Scan(ctx, c, db); err != nil {
		t.Fatal(err)
	}
	second := &fakeLLM{reply: reply}
	result, err := RunProgress(ctx, c, db, Generators{Analysis: second}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Processed != 1 {
		t.Fatalf("lexically unrelated corpus re-analyzed %d docs, want 1", result.Processed)
	}
}
