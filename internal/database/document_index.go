package database

import "context"

// DocumentIndexItem contains only the fields displayed by the review UI's
// document table. Full document records remain available through Documents.
type DocumentIndexItem struct {
	Path              string `json:"path"`
	SHA256            string `json:"sha256"`
	AnalyzedSHA256    string `json:"analyzed_sha256"`
	AnalyzedModel     string `json:"analyzed_model"`
	FmSHA256          string `json:"fm_sha256"`
	BodySHA256        string `json:"body_sha256"`
	SeenFmSHA256      string `json:"seen_fm_sha256"`
	SeenBodySHA256    string `json:"seen_body_sha256"`
	Title             string `json:"title"`
	TagsJSON          string `json:"tags_json"`
	Status            string `json:"status"`
	FrontmatterJSON   string `json:"frontmatter_json"`
	FrontmatterIssues string `json:"frontmatter_issues"`
}

// DocumentIndexPage reads one ordered slice without loading the full analysis
// lineage, signatures, or embeddings for every document into the web server.
func (d *DB) DocumentIndexPage(ctx context.Context, limit, offset int) ([]DocumentIndexItem, int, error) {
	var total int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents`).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := d.QueryContext(ctx, `SELECT path,sha256,analyzed_sha256,analyzed_model,
fm_sha256,body_sha256,seen_fm_sha256,seen_body_sha256,title,tags_json,status,
frontmatter_json,frontmatter_issues FROM documents ORDER BY path LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []DocumentIndexItem{}
	for rows.Next() {
		var item DocumentIndexItem
		if err := rows.Scan(&item.Path, &item.SHA256, &item.AnalyzedSHA256, &item.AnalyzedModel,
			&item.FmSHA256, &item.BodySHA256, &item.SeenFmSHA256, &item.SeenBodySHA256,
			&item.Title, &item.TagsJSON, &item.Status, &item.FrontmatterJSON, &item.FrontmatterIssues); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}
