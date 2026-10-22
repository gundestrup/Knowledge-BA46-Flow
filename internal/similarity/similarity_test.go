package similarity

import (
	"strings"
	"testing"
)

func TestIdenticalDocumentsScoreOne(t *testing.T) {
	text := strings.Repeat("the quick brown fox jumps over the lazy dog ", 20)
	sig := Signature(text)
	if len(sig) == 0 {
		t.Fatal("empty signature")
	}
	if got := Estimate(sig, sig); got != 1.0 {
		t.Fatalf("self similarity=%f", got)
	}
}

func TestDifferentDocumentsScoreLow(t *testing.T) {
	a := Signature(strings.Repeat("kubernetes pods deployments services ingress ", 20))
	b := Signature(strings.Repeat("sourdough starter flour hydration fermentation ", 20))
	if got := Estimate(a, b); got > 0.1 {
		t.Fatalf("unrelated similarity=%f", got)
	}
}

func TestNearDuplicatesScoreHigh(t *testing.T) {
	base := strings.Repeat("install the package then configure the daemon and restart ", 20)
	a := Signature(base + "see the troubleshooting section")
	b := Signature(base + "see the appendix for details")
	if got := Estimate(a, b); got < 0.5 {
		t.Fatalf("near-duplicate similarity=%f", got)
	}
}

func TestEmbeddingMarshalRoundTrip(t *testing.T) {
	embedding := []float32{0.5, -1.25, 3.14159, 0}
	got := UnmarshalEmbedding(MarshalEmbedding(embedding))
	if len(got) != len(embedding) {
		t.Fatalf("round trip length %d, want %d", len(got), len(embedding))
	}
	for i := range embedding {
		if got[i] != embedding[i] {
			t.Fatalf("element %d: got %f, want %f", i, got[i], embedding[i])
		}
	}
	for _, malformed := range [][]byte{nil, {}, {1, 2, 3}, {1, 2, 3, 4, 5}} {
		if got := UnmarshalEmbedding(malformed); got != nil {
			t.Fatalf("malformed %v decoded to %v", malformed, got)
		}
	}
}

func TestCosine(t *testing.T) {
	unit := []float32{0.6, 0.8} // |v| = 1
	if got := Cosine(unit, unit); got != 1.0 {
		t.Errorf("identical vectors: %f, want 1", got)
	}
	if got := Cosine([]float32{1, 0}, []float32{0, 1}); got != 0 {
		t.Errorf("orthogonal vectors: %f, want 0", got)
	}
	if got := Cosine([]float32{1, 0}, []float32{-1, 0}); got != 0 {
		t.Errorf("opposing vectors clamp to 0, got %f", got)
	}
	if got := Cosine([]float32{1}, []float32{1, 2}); got != 0 {
		t.Errorf("mismatched lengths: %f, want 0", got)
	}
	if got := Cosine(nil, nil); got != 0 {
		t.Errorf("empty vectors: %f, want 0", got)
	}
	if got := Cosine([]float32{0, 0}, []float32{1, 1}); got != 0 {
		t.Errorf("zero norm: %f, want 0", got)
	}
	// Non-unit magnitudes normalize: 2v·v scores 1.
	if got := Cosine([]float32{2, 0}, []float32{4, 0}); got != 1.0 {
		t.Errorf("scaled vectors: %f, want 1", got)
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	sig := Signature("one two three four five six seven eight nine ten")
	got := Unmarshal(Marshal(sig))
	if len(got) != len(sig) {
		t.Fatalf("round trip: %v vs %v", got, sig)
	}
	for i := range got {
		if got[i] != sig[i] {
			t.Fatalf("round trip: %v vs %v", got, sig)
		}
	}
	if Unmarshal([]byte{1, 2, 3}) != nil || Unmarshal(nil) != nil {
		t.Fatal("malformed input should unmarshal to nil")
	}
}

func TestShortAndEmptyDocuments(t *testing.T) {
	if sig := Signature(""); len(sig) != 0 {
		t.Fatalf("empty input: %v", sig)
	}
	// Fewer than shingleWords tokens produce no shingles — estimation is 0.
	a := Signature("one two")
	b := Signature("one two")
	if got := Estimate(a, b); got != 0 {
		t.Fatalf("sub-shingle docs: %f", got)
	}
	if got := Estimate(nil, b); got != 0 {
		t.Fatalf("nil sketch: %f", got)
	}
}

func TestIncrementalMatchesOneShot(t *testing.T) {
	text := "alpha beta gamma delta epsilon zeta eta theta iota kappa"
	sk := NewSketcher()
	for _, w := range strings.Fields(text) {
		sk.AddWord(w)
	}
	one := Signature(text)
	inc := sk.Signature()
	if len(one) != len(inc) {
		t.Fatalf("incremental=%v one-shot=%v", inc, one)
	}
	for i := range one {
		if one[i] != inc[i] {
			t.Fatalf("incremental=%v one-shot=%v", inc, one)
		}
	}
}

// BenchmarkSketch pins the per-document sketching cost — the scanner runs this
// over every markdown file on every scan.
func BenchmarkSketch(b *testing.B) {
	paragraph := "the quick brown fox jumps over the lazy dog while the knowledge base grows with every scanned markdown document "
	var doc strings.Builder
	for range 200 {
		doc.WriteString(paragraph)
	}
	text := doc.String()
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		Signature(text)
	}
}

func BenchmarkSignature(b *testing.B) {
	body := strings.Repeat("Knowledge note body: tags links frontmatter similarity hashing shingles. ", 60)
	for b.Loop() {
		Signature(body)
	}
}

func BenchmarkCosine(b *testing.B) {
	a := make([]float32, 768)
	c := make([]float32, 768)
	for i := range a {
		a[i], c[i] = float32(i%7)*0.1, float32(i%5)*0.2
	}
	for b.Loop() {
		Cosine(a, c)
	}
}
