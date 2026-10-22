// Package similarity computes compact content sketches so the analyzer can
// rank merge/relation candidates by real text overlap instead of path names
// alone. The sketch is a bottom-k minwise signature over word 3-shingles:
// hashing each shingle and keeping the k smallest values yields an
// order-independent Jaccard estimator that compares in O(k) — cheap enough to
// score a whole corpus per analyzed document without any LLM involvement.
//
// The sketch is computed and stored locally; it is a storage-level index, not
// tied to SQLite — any database can persist the opaque []byte form.
package similarity

import (
	"encoding/binary"
	"hash/fnv"
	"math"
	"slices"
	"strings"
	"unicode"
)

// SketchSize is the number of minimum hashes retained per document. 64 gives
// ~12% standard error on the Jaccard estimate — plenty for flagging duplicates
// and ranking candidates.
const SketchSize = 64

// shingleWords is the shingle width in normalized words. 3-word shingles
// balance sensitivity (short docs still produce shingles) and specificity
// (shared boilerplate like frontmatter keys rarely aligns 3-for-3).
const shingleWords = 3

// Sketcher builds a signature incrementally as words stream by — the scanner
// feeds it while hashing the file, so the corpus is sketched for free.
type Sketcher struct {
	mins []uint64 // sorted, smallest first, deduplicated
	seen map[uint64]bool
	tail []string // last shingleWords-1 normalized words
}

// NewSketcher returns an empty sketcher.
func NewSketcher() *Sketcher {
	return &Sketcher{seen: map[uint64]bool{}}
}

// AddWord feeds one raw word (whitespace-delimited token) into the sketch.
// Normalization happens here: lowercase, and non-letter/digit edges trimmed.
func (s *Sketcher) AddWord(word string) {
	w := normalize(word)
	if w == "" {
		return
	}
	s.tail = append(s.tail, w)
	if len(s.tail) < shingleWords {
		return
	}
	s.tail = s.tail[len(s.tail)-shingleWords:]
	s.add(strings.Join(s.tail, " "))
}

// Signature emits the accumulated signature: the SketchSize smallest shingle
// hashes, sorted ascending. Fewer than SketchSize entries means the document
// simply produced fewer shingles.
func (s *Sketcher) Signature() []uint64 {
	return slices.Clone(s.mins)
}

// Signature is the one-shot variant for callers holding the whole text.
func Signature(text string) []uint64 {
	sk := NewSketcher()
	for _, w := range strings.Fields(text) {
		sk.AddWord(w)
	}
	return sk.Signature()
}

// add folds one shingle into the bottom-k sketch.
func (s *Sketcher) add(shingle string) {
	h := fnv.New64a()
	_, _ = h.Write([]byte(shingle))
	v := h.Sum64()
	if s.seen[v] {
		return
	}
	s.seen[v] = true
	if len(s.mins) < SketchSize {
		s.mins = append(s.mins, v)
		slices.Sort(s.mins)
		return
	}
	if v >= s.mins[len(s.mins)-1] {
		delete(s.seen, v) // never entered the sketch; don't leak seen entries
		return
	}
	delete(s.seen, s.mins[len(s.mins)-1])
	s.mins[len(s.mins)-1] = v
	slices.Sort(s.mins)
}

// Estimate returns an approximate Jaccard index in [0,1] between two
// signatures. For sketches smaller than SketchSize the estimator degrades
// gracefully toward the exact overlap of the retained shingles.
func Estimate(a, b []uint64) float64 {
	inter := 0
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			inter++
			i++
			j++
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// Marshal encodes a signature as little-endian u64s for BLOB storage.
func Marshal(sig []uint64) []byte {
	b := make([]byte, len(sig)*8)
	for i, v := range sig {
		binary.LittleEndian.PutUint64(b[i*8:], v)
	}
	return b
}

// MarshalEmbedding encodes a float32 embedding vector as little-endian f32s
// for BLOB storage — same DB-agnostic pattern as Marshal.
func MarshalEmbedding(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// UnmarshalEmbedding decodes a marshaled embedding; malformed input (length
// not a multiple of 4) yields nil.
func UnmarshalEmbedding(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// Cosine returns the cosine similarity of two embedding vectors, clamped to
// [0,1]: embeddings from Ollama's /api/embed are unit-normalized, so negative
// values only appear for genuinely opposing directions — noise, not signal.
// Empty or mismatched vectors score 0.
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	c := dot / math.Sqrt(na*nb)
	return max(c, 0)
}

// Unmarshal decodes a marshaled signature; malformed input (length not a
// multiple of 8) yields nil rather than a half-decoded sketch.
func Unmarshal(b []byte) []uint64 {
	if len(b) == 0 || len(b)%8 != 0 {
		return nil
	}
	sig := make([]uint64, len(b)/8)
	for i := range sig {
		sig[i] = binary.LittleEndian.Uint64(b[i*8:])
	}
	return sig
}

// normalize lowercases and trims non-letter/digit edges so punctuation and
// markdown syntax don't fracture otherwise identical words.
func normalize(word string) string {
	return strings.TrimFunc(strings.ToLower(word), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}
