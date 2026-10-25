// Package websearch adapts Ollama's hosted web search API
// (POST https://ollama.com/api/web_search) to the single Search call the
// freshness verify stage needs. It is plain net/http over the documented
// wire format — no SDK — and it is a deliberate egress boundary: the API is
// hosted by Ollama, not the local server ollama_url points at, so claim text
// sent here leaves the machine regardless of how local the models are.
// Callers therefore construct a client only behind an explicit opt-in
// (--web-verify / web_verify) plus an OLLAMA_API_KEY.
package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is Ollama's hosted search endpoint root. Unlike the model
// server this is never localhost — that is exactly why verification needs
// explicit consent rather than the model lanes' locality checks.
const DefaultBaseURL = "https://ollama.com"

const (
	// requestTimeout bounds one search call; freshness verifies flags
	// sequentially, so a hung request must not stall the pass.
	requestTimeout = 30 * time.Second
	// maxResults is the server's documented cap (1..10).
	maxResults = 10
	// maxResponseBytes bounds a reply that should never exceed a few KB of
	// JSON; the cap keeps a misbehaving endpoint from streaming endlessly.
	maxResponseBytes = 4 << 20
)

// Result is one search hit — title, URL and the snippet the API returns.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

// StatusError is a non-2xx response from the search API, carrying the
// server's own error field when present.
type StatusError struct {
	StatusCode int
	Status     string
	Message    string
}

func (e StatusError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("web search %s (HTTP %d)", e.Message, e.StatusCode)
	}
	return fmt.Sprintf("web search %s", e.Status)
}

// Client wraps the search endpoint plus the bearer key.
type Client struct {
	base string
	key  string
	http *http.Client
}

// New builds a client against the hosted endpoint. An empty key is accepted
// so construction stays lazy — Search then fails closed with a clear error
// instead of sending an unauthenticated request.
func New(apiKey string) *Client {
	return &Client{
		base: DefaultBaseURL,
		key:  apiKey,
		http: &http.Client{
			Timeout:       requestTimeout,
			CheckRedirect: rejectCrossOriginRedirect,
		},
	}
}

// WithBaseURL overrides the endpoint — for tests and compatible providers.
func (c *Client) WithBaseURL(rawURL string) *Client {
	c.base = strings.TrimRight(rawURL, "/")
	return c
}

func rejectCrossOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	previous := via[len(via)-1].URL
	if req.URL.User != nil ||
		!strings.EqualFold(req.URL.Scheme, previous.Scheme) ||
		!strings.EqualFold(req.URL.Host, previous.Host) {
		return errors.New("cross-origin redirect blocked")
	}
	return nil
}

// Search runs one query and returns the hits the API reports, capped at the
// server's documented maximum. Results are third-party web content — the
// caller treats them as untrusted prompt input, like repository text.
func (c *Client) Search(ctx context.Context, query string, n int) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("empty web search query")
	}
	if c.key == "" {
		return nil, errors.New("web search requires OLLAMA_API_KEY")
	}
	if _, err := url.Parse(c.base + "/"); err != nil {
		return nil, fmt.Errorf("web search URL: %w", err)
	}
	if n <= 0 {
		n = 5
	}
	if n > maxResults {
		n = maxResults
	}
	payload, err := json.Marshal(struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}{Query: query, MaxResults: n})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/web_search", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.key)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("web search request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := io.LimitReader(resp.Body, maxResponseBytes)
	if resp.StatusCode/100 != 2 {
		var er struct {
			Error string `json:"error"`
		}
		data, _ := io.ReadAll(body)
		_ = json.Unmarshal(data, &er)
		return nil, StatusError{StatusCode: resp.StatusCode, Status: resp.Status, Message: strings.TrimSpace(er.Error)}
	}
	var out struct {
		Results []Result `json:"results"`
		Error   string   `json:"error"`
	}
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode web search response: %w", err)
	}
	if out.Error != "" {
		return nil, errors.New(out.Error)
	}
	if len(out.Results) > maxResults {
		out.Results = out.Results[:maxResults]
	}
	return out.Results, nil
}
