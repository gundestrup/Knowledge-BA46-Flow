// Package ollama adapts the Ollama HTTP API to the single /api/generate
// call shape the analyzer needs. The client is plain net/http over the
// documented wire format — no external SDK — so this module's dependency
// tree does not include the Ollama server codebase.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// StatusError is a non-2xx response from the Ollama server. The message is
// the server's own error field when present, otherwise the status line.
type StatusError struct {
	StatusCode int
	Status     string
	Message    string
}

func (e StatusError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("ollama %s (HTTP %d)", e.Message, e.StatusCode)
	}
	return fmt.Sprintf("ollama %s", e.Status)
}

// Client wraps one Ollama server URL + model pair; http.Client carries the timeout.
type Client struct {
	model              string
	numCtx             int
	maxOutputTokens    int
	think              bool
	keepAlive          string // duration string ("5m0s"); empty omits the field
	base               string
	http               *http.Client
	err                error
	ctxOnce            sync.Once
	ctxLen             int
	limitMu            sync.Mutex
	requiredCapability string
	allowRemote        bool
	checkOnce          sync.Once
	checkErr           error
}

const (
	// minAutoNumCtx floors auto-sized windows — below Ollama's own default a
	// request would gain nothing and small prompts still need reply room.
	minAutoNumCtx = 4096
	// fallbackCtxLen caps auto sizing when /api/show cannot report the
	// model's context_length (older servers, unreachable server at first call).
	fallbackCtxLen = 16384
	// bytesPerToken estimates prompt tokens from prompt bytes; ~4 holds for
	// English technical markdown and errs slightly high for code, which is
	// the safe direction for window sizing.
	bytesPerToken = 4
	// replyHeadroom covers the JSON envelope and non-content fields on top of
	// the document-sized reply expansion.
	replyHeadroom = 1024
	// outputLimitRetryFactor grows a bounded num_predict on a length-truncated
	// reply — verbose/cloud models can exceed conservative metadata caps even
	// with think off. One retry per call; the proven cap is kept for later
	// requests so the client learns its model's real reply size.
	outputLimitRetryFactor = 4
	// learnedOutputLimitMax bounds the cap the client may learn on its own;
	// beyond it a truncated reply errors instead of silently ballooning cost.
	learnedOutputLimitMax = 8192
)

// New builds a client. timeout is the per-request HTTP timeout in seconds;
// numCtx is the context window sent as options.num_ctx: a positive value pins
// it, while <= 0 auto-sizes it per request from the prompt size
// (~prompt_tokens x 2 — expand/merge replies return a complete rewritten
// document — plus headroom), floored at 4096 and capped by the model's
// context_length from /api/show (fallback 16384 if that query fails).
// think enables
// reasoning-model chain-of-thought; for the strict JSON contracts used here
// it buys nothing and multiplies latency, so callers normally pass false —
// the request then sends think:false explicitly, which reasoning models such
// as qwen3 honor while other models ignore it. keepAlive is sent as the
// request's keep_alive — "0" unloads the model right after the call (useful
// on memory-constrained hosts when several large models alternate); empty
// means Ollama's default.
func New(rawURL, model string, timeout, numCtx int, think bool, keepAlive string) *Client {
	_, err := url.Parse(strings.TrimRight(rawURL, "/") + "/")
	c := &Client{
		model:  model,
		numCtx: numCtx,
		think:  think,
		base:   strings.TrimRight(rawURL, "/"),
		err:    err,
		http: &http.Client{
			Timeout:       time.Duration(timeout) * time.Second,
			CheckRedirect: rejectCrossOriginRedirect,
		},
	}
	if keepAlive != "" {
		if d, perr := time.ParseDuration(keepAlive); perr == nil {
			c.keepAlive = d.String()
		}
	}
	return c
}

func rejectCrossOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 Ollama redirects")
	}
	previous := via[len(via)-1].URL
	if req.URL.User != nil ||
		!strings.EqualFold(req.URL.Scheme, previous.Scheme) ||
		!strings.EqualFold(req.URL.Host, previous.Host) {
		return errors.New("cross-origin Ollama redirect blocked")
	}
	return nil
}

// WithOutputLimit configures a metadata-only client before its first use.
// A positive limit also avoids reserving a document-sized reply context.
func (c *Client) WithOutputLimit(tokens int) *Client {
	c.maxOutputTokens = tokens
	return c
}

// Wire types — the fields this client sends and reads back.
type generateRequest struct {
	Model     string          `json:"model"`
	Prompt    string          `json:"prompt"`
	Stream    bool            `json:"stream"`
	Format    json.RawMessage `json:"format,omitempty"`
	Think     bool            `json:"think"`
	KeepAlive string          `json:"keep_alive,omitempty"`
	Options   map[string]any  `json:"options,omitempty"`
}

// generateResponse is one line of the (possibly streaming) reply; Ollama
// reports per-chunk errors inside a 200 response via the error field.
type generateResponse struct {
	Response   string `json:"response"`
	DoneReason string `json:"done_reason"`
	Error      string `json:"error"`
}

// post issues one JSON POST and decodes a single-object reply into out.
// Non-2xx statuses surface as StatusError carrying the server's error field.
func (c *Client) post(ctx context.Context, path string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.doJSON(req, out)
}

func (c *Client) doJSON(req *http.Request, out any) error {
	if c.err != nil {
		return fmt.Errorf("ollama URL: %w", c.err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ollama request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		var er struct {
			Error string `json:"error"`
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = json.Unmarshal(data, &er)
		return StatusError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Message:    strings.TrimSpace(er.Error),
		}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

// Generate sends a prompt and returns the trimmed response text. `format` is the
// raw value of the request's "format" field — a JSON Schema for structured
// outputs, `"json"` for plain JSON mode, or nil to omit it.
//
// Stream:false means the whole reply normally arrives as one JSON object;
// the response body is still decoded as a line-delimited stream so servers
// that chunk the reply anyway are handled identically.
func (c *Client) Generate(ctx context.Context, prompt string, format json.RawMessage) (string, error) {
	if c.err != nil {
		return "", fmt.Errorf("ollama URL: %w", c.err)
	}
	if err := c.checkModel(ctx, CapabilityCompletion); err != nil {
		return "", err
	}
	out, doneReason, err := c.generate(ctx, prompt, format, c.outputLimit())
	if err != nil {
		return "", err
	}
	if doneReason != "length" {
		return out, nil
	}
	retry, ok := c.grownLimit()
	if !ok {
		return "", errors.New("ollama response was truncated by the context or generation limit")
	}
	out, doneReason, err = c.generate(ctx, prompt, format, retry)
	if err != nil {
		return "", err
	}
	if doneReason == "length" {
		return "", errors.New("ollama response was truncated by the context or generation limit")
	}
	c.commitLimit(retry)
	return out, nil
}

// outputLimit reports the current num_predict cap, including learned growth.
func (c *Client) outputLimit() int {
	c.limitMu.Lock()
	defer c.limitMu.Unlock()
	return c.maxOutputTokens
}

// grownLimit proposes the cap for a single retry after a truncated reply;
// ok=false when the client has no bounded cap or is already at the ceiling.
func (c *Client) grownLimit() (int, bool) {
	c.limitMu.Lock()
	defer c.limitMu.Unlock()
	if c.maxOutputTokens <= 0 || c.maxOutputTokens >= learnedOutputLimitMax {
		return 0, false
	}
	return min(c.maxOutputTokens*outputLimitRetryFactor, learnedOutputLimitMax), true
}

// commitLimit adopts a cap that proved sufficient — subsequent requests skip
// the truncated first attempt. max() guards concurrent growth.
func (c *Client) commitLimit(tokens int) {
	c.limitMu.Lock()
	c.maxOutputTokens = max(c.maxOutputTokens, tokens)
	c.limitMu.Unlock()
}

// generate performs one request/response round and reports the reply's final
// done_reason; "length" means context or num_predict cut the reply short.
func (c *Client) generate(ctx context.Context, prompt string, format json.RawMessage, numPredict int) (string, string, error) {
	numCtx := c.numCtx
	if numCtx <= 0 {
		numCtx = c.autoNumCtx(ctx, prompt, numPredict)
	}
	options := map[string]any{"num_ctx": numCtx}
	if len(format) > 0 {
		options["temperature"] = 0
	}
	if numPredict > 0 {
		options["num_predict"] = numPredict
	}
	payload := generateRequest{
		Model:     c.model,
		Prompt:    prompt,
		Stream:    false,
		Format:    format,
		Think:     c.think,
		KeepAlive: c.keepAlive,
		Options:   options,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", "", fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return "", "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("ollama request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		var er struct {
			Error string `json:"error"`
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = json.Unmarshal(data, &er)
		return "", "", StatusError{StatusCode: resp.StatusCode, Status: resp.Status, Message: strings.TrimSpace(er.Error)}
	}
	var out strings.Builder
	var doneReason string
	dec := json.NewDecoder(resp.Body)
	for {
		var chunk generateResponse
		if err := dec.Decode(&chunk); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", "", fmt.Errorf("decode ollama response: %w", err)
		}
		if chunk.Error != "" {
			return "", "", errors.New(chunk.Error)
		}
		doneReason = chunk.DoneReason
		out.WriteString(chunk.Response)
	}
	return strings.TrimSpace(out.String()), doneReason, nil
}

// autoNumCtx sizes the window for one prompt: twice the estimated prompt
// tokens (the reply may contain the whole rewritten document) plus headroom,
// floored and capped by the model's real context_length. A bounded reply
// (numPredict > 0) needs only the smaller metadata-sized allowance.
func (c *Client) autoNumCtx(ctx context.Context, prompt string, numPredict int) int {
	want := len(prompt)/bytesPerToken*2 + replyHeadroom
	if numPredict > 0 {
		want = len(prompt)/bytesPerToken + numPredict + replyHeadroom
	}
	if want < minAutoNumCtx {
		want = minAutoNumCtx
	}
	if max := c.modelContextLength(ctx); want > max {
		want = max
	}
	return want
}

// modelContextLength queries /api/show once per client and caches the model's
// context_length (e.g. "qwen3.context_length"); on any failure it returns the
// conservative fallback so generate calls never depend on the show endpoint.
func (c *Client) modelContextLength(ctx context.Context) int {
	c.ctxOnce.Do(func() {
		c.ctxLen = fallbackCtxLen
		var resp struct {
			ModelInfo map[string]any `json:"model_info"`
		}
		if err := c.post(ctx, "/api/show", map[string]string{"model": c.model}, &resp); err != nil {
			return
		}
		max := 0
		for k, v := range resp.ModelInfo {
			if strings.HasSuffix(k, ".context_length") {
				if n, ok := v.(float64); ok && int(n) > max {
					max = int(n)
				}
			}
		}
		if max > 0 {
			c.ctxLen = max
		}
	})
	return c.ctxLen
}

// Unload releases a completed pipeline stage without forcing a reload after
// every document. The empty prompt and zero keep_alive are Ollama's unload API.
func (c *Client) Unload(ctx context.Context) error {
	if c.err != nil {
		return fmt.Errorf("ollama URL: %w", c.err)
	}
	var ignored generateResponse
	err := c.post(ctx, "/api/generate", generateRequest{
		Model: c.model, Stream: false, KeepAlive: "0s",
	}, &ignored)
	// Unloading is idempotent: a missing optional model cannot be resident,
	// and its generation error must not prevent the analysis model loading.
	var statusErr StatusError
	if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}

// Embed returns the model's embedding vector for one text via /api/embed.
// Input is truncated server-side to the model's context (Truncate:true) so
// oversized documents embed as their prefix rather than failing.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	if c.err != nil {
		return nil, fmt.Errorf("ollama URL: %w", c.err)
	}
	if err := c.checkModel(ctx, CapabilityEmbedding); err != nil {
		return nil, err
	}
	var resp struct {
		Embeddings [][]float32 `json:"embeddings"`
		Error      string      `json:"error"`
	}
	err := c.post(ctx, "/api/embed", struct {
		Model     string `json:"model"`
		Input     string `json:"input"`
		Truncate  bool   `json:"truncate"`
		KeepAlive string `json:"keep_alive,omitempty"`
	}{Model: c.model, Input: text, Truncate: true, KeepAlive: c.keepAlive}, &resp)
	if err != nil {
		var statusErr StatusError
		if errors.As(err, &statusErr) {
			return nil, statusErr
		}
		return nil, err
	}
	if resp.Error != "" {
		return nil, errors.New(resp.Error)
	}
	if len(resp.Embeddings) != 1 || len(resp.Embeddings[0]) == 0 {
		return nil, errors.New("ollama embed: empty response")
	}
	for _, value := range resp.Embeddings[0] {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, errors.New("ollama embed: non-finite vector")
		}
	}
	return resp.Embeddings[0], nil
}
