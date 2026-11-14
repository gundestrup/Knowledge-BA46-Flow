package database

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"unicode"
)

const ftsQueryTerms = 24
const createDocumentFTS = `CREATE VIRTUAL TABLE IF NOT EXISTS document_fts USING fts5(
 path UNINDEXED, source_sha256 UNINDEXED, title, summary, tags, body,
 tokenize='unicode61 remove_diacritics 2'
)`

func (d *DB) ensureFTS(ctx context.Context) error {
	_, err := d.ExecContext(ctx, createDocumentFTS)
	if ftsUnavailable(err) {
		return nil
	}
	return err
}

// IndexSearchDocument updates the lexical index only when the source version
// changes. The FTS table is derived state; it never alters analysis stamps.
func (d *DB) IndexSearchDocument(ctx context.Context, path, sourceSHA, title, summary, tags, body string) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT source_sha256 FROM document_fts WHERE path=?`, path).Scan(&existing)
	if ftsUnavailable(err) {
		return nil
	}
	if err == nil && existing == sourceSHA {
		return tx.Commit()
	}
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM document_fts WHERE path=?`, path); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO document_fts(path,source_sha256,title,summary,tags,body)
VALUES(?,?,?,?,?,?)`, path, sourceSHA, title, summary, tags, body); err != nil {
		return err
	}
	return tx.Commit()
}

// SearchCandidateRanks returns normalized lexical ranks for a bounded set of
// documents. Scores are relative to this query; they are not probabilities.
func (d *DB) SearchCandidateRanks(ctx context.Context, text, excludePath string, limit int) (map[string]float64, error) {
	query := ftsMatchQuery(text)
	if query == "" || limit <= 0 {
		return nil, nil
	}
	rows, err := d.QueryContext(ctx, `SELECT path FROM document_fts
WHERE document_fts MATCH ? AND path<>?
ORDER BY bm25(document_fts,0,0,5,3,2,1),path LIMIT ?`, query, excludePath, limit)
	if ftsUnavailable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ranks := make(map[string]float64, limit)
	for rank := 1; rows.Next(); rank++ {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		ranks[path] = 1 / float64(rank)
	}
	return ranks, rows.Err()
}

func ftsUnavailable(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "no such module: fts5") ||
		strings.Contains(message, "no such table: document_fts")
}

func ftsMatchQuery(text string) string {
	frequencies := map[string]int{}
	var token strings.Builder
	flush := func() {
		word := strings.ToLower(token.String())
		token.Reset()
		if len([]rune(word)) >= 3 {
			frequencies[word]++
		}
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			token.WriteRune(r)
		} else if token.Len() > 0 {
			flush()
		}
	}
	if token.Len() > 0 {
		flush()
	}
	terms := make([]string, 0, len(frequencies))
	for word := range frequencies {
		terms = append(terms, word)
	}
	slices.SortFunc(terms, func(a, b string) int {
		if frequencies[a] != frequencies[b] {
			return frequencies[b] - frequencies[a]
		}
		return strings.Compare(a, b)
	})
	if len(terms) > ftsQueryTerms {
		terms = terms[:ftsQueryTerms]
	}
	for i, term := range terms {
		terms[i] = `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
	}
	return strings.Join(terms, " OR ")
}
