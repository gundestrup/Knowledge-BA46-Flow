// Package linkcheck implements the opt-in external link check: it extracts
// http(s) URLs from document bodies, probes them over the network and
// records the verdict in each document's tool-owned `kbm.link_check`
// frontmatter block. The check is the one documented carve-out that writes
// files outside `apply`: the kbm block is operational metadata KBM-flow
// owns, and the canonical content hash ignores it — recording a check never
// requeues analysis or invalidates suggestions.
package linkcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/links"
	"knowledge-ba46-flow/internal/paths"
)

// Outcome buckets. Only definitive answers count as dead — anything a bot
// wall, rate limit or flaky network could explain lands in unverified.
const (
	OK         = "ok"
	Dead       = "dead"
	Unverified = "unverified"
)

// Result summarizes one run for the job detail line and UI.
type Result struct {
	Documents  int `json:"documents"`  // documents containing external URLs
	URLs       int `json:"urls"`       // unique URLs probed
	Dead       int `json:"dead"`       // unique URLs that answered 404/410
	Unverified int `json:"unverified"` // unique URLs that could not be confirmed
	Written    int `json:"written"`    // documents whose kbm.link_check block was updated
	Failed     int `json:"failed"`     // documents that could not be read or updated
}

// Summary renders the one-line job/progress detail.
func (r Result) Summary() string {
	return fmt.Sprintf("Checked %d external link(s) across %d document(s); %d dead, %d unverified, %d updated.",
		r.URLs, r.Documents, r.Dead, r.Unverified, r.Written)
}

const (
	requestTimeout = 20 * time.Second
	workers        = 8
	userAgent      = "KBM-flow link checker"
)

// Run probes every external URL referenced by the corpus and records the
// verdicts. Unique URLs are probed once and the outcome is shared across the
// documents that reference them.
func Run(ctx context.Context, c config.Config, db *database.DB) (Result, error) {
	root, err := os.OpenRoot(c.RepositoryPath)
	if err != nil {
		return Result{}, err
	}
	defer root.Close()
	docs, err := db.Documents(ctx)
	if err != nil {
		return Result{}, err
	}
	// Extract per-document URL sets and the unique probe list in one pass.
	docURLs := map[string][]string{}
	var unique []string
	seen := map[string]bool{}
	for _, doc := range docs {
		if doc.Status == "missing" {
			continue
		}
		body, err := readBody(root, doc.Path)
		if err != nil {
			continue // unreadable docs are reported through scan, not here
		}
		urls := links.ExternalURLs(body)
		if len(urls) == 0 {
			continue
		}
		docURLs[doc.Path] = urls
		for _, u := range urls {
			if !seen[u] {
				seen[u] = true
				unique = append(unique, u)
			}
		}
	}
	out := Result{Documents: len(docURLs), URLs: len(unique)}
	if len(unique) == 0 {
		return out, nil
	}

	verdicts := probeAll(ctx, unique)
	now := time.Now().UTC().Format(time.RFC3339)
	for path, urls := range docURLs {
		var dead, unverified []string
		for _, u := range urls {
			switch verdicts[u] {
			case Dead:
				dead = append(dead, u)
			case Unverified:
				unverified = append(unverified, u)
			}
		}
		if err := record(ctx, root, db, path, now, dead, unverified); err != nil {
			out.Failed++
			continue
		}
		out.Written++
	}
	for _, v := range verdicts {
		switch v {
		case Dead:
			out.Dead++
		case Unverified:
			out.Unverified++
		}
	}
	return out, nil
}

// readBody reads a document confined to the repository root and returns its
// body (post-frontmatter text). Documents that fail to read or parse are
// skipped by the caller.
func readBody(root *os.Root, rel string) (string, error) {
	_, body, err := readParsed(root, rel)
	return body, err
}

// readParsed is readBody plus the parsed frontmatter fields.
func readParsed(root *os.Root, rel string) (map[string]any, string, error) {
	local, err := paths.CleanRelative(rel)
	if err != nil {
		return nil, "", err
	}
	f, err := root.Open(local)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, "", err
	}
	return frontmatter.Parse(string(raw))
}

// record writes the kbm.link_check verdict block into the document — the one
// permitted file mutation outside apply, for tool-owned operational
// metadata — then refreshes the stored parse so the dashboard sees it. The
// canonical content hash cannot change, so nothing requeues analysis.
func record(ctx context.Context, root *os.Root, db *database.DB, rel, at string, dead, unverified []string) error {
	if err := frontmatter.WriteManaged(root, rel, "link_check", map[string]any{
		"at":         at,
		"dead":       dead,
		"unverified": unverified,
	}); err != nil {
		return err
	}
	fields, _, err := readParsed(root, rel)
	if err != nil {
		return err
	}
	fmJSON, _ := json.Marshal(fields)
	issues, _ := json.Marshal(frontmatter.Validate(fields))
	return db.RefreshFrontmatter(ctx, rel, string(fmJSON), string(issues))
}

// probeAll checks every URL on a small worker pool; a shared URL is probed
// once and its verdict reused for every referencing document.
func probeAll(ctx context.Context, urls []string) map[string]string {
	client := &http.Client{Timeout: requestTimeout}
	jobs := make(chan string)
	results := make(chan [2]string, len(urls))
	for i := 0; i < workers; i++ {
		go func() {
			for u := range jobs {
				results <- [2]string{u, probe(ctx, client, u)}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, u := range urls {
			select {
			case jobs <- u:
			case <-ctx.Done():
				return
			}
		}
	}()
	out := make(map[string]string, len(urls))
	for range urls {
		select {
		case r := <-results:
			out[r[0]] = r[1]
		case <-ctx.Done():
			return out
		}
	}
	return out
}

// probe resolves one URL to a verdict. HEAD first; servers that refuse the
// method get a minimal GET instead. Redirects are followed by the client.
func probe(ctx context.Context, client *http.Client, url string) string {
	status, err := request(ctx, client, http.MethodHead, url)
	if errors.Is(err, errMethodNotAllowed) {
		status, err = request(ctx, client, http.MethodGet, url)
	}
	if err != nil {
		return Unverified
	}
	return classify(status)
}

var errMethodNotAllowed = errors.New("method not allowed")

// request issues one bounded request; response bodies are discarded — the
// status line is the whole answer.
func request(ctx context.Context, client *http.Client, method, url string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	if method == http.MethodGet {
		req.Header.Set("Range", "bytes=0-0")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotImplemented {
		return 0, errMethodNotAllowed
	}
	return resp.StatusCode, nil
}

// classify maps a status code to a verdict: only unambiguous "gone" codes
// are dead; auth walls, rate limits, other 4xx and 5xx stay unverified so a
// cautious reader, not the tool, decides what to trust.
func classify(status int) string {
	switch {
	case status >= 200 && status < 400:
		return OK
	case status == http.StatusNotFound || status == http.StatusGone:
		return Dead
	default:
		return Unverified
	}
}
