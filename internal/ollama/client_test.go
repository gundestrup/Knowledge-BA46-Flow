package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// wire mirrors the fields of the /api/generate request and response that the
// client sends and reads.
type wire struct {
	Model     string          `json:"model"`
	Prompt    string          `json:"prompt"`
	Stream    bool            `json:"stream"`
	Format    json.RawMessage `json:"format"`
	Think     *bool           `json:"think"`
	KeepAlive string          `json:"keep_alive"`
	Options   map[string]any  `json:"options"`
	Response  string          `json:"response"`
	Error     string          `json:"error"`
}

func TestGenerate(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var req wire
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Model != "test-model" || req.Prompt != "hello" || req.Stream || string(req.Format) != `{"type":"object"}` {
			t.Fatalf("unexpected request: %+v", req)
		}
		if req.Options["num_ctx"] != float64(8192) {
			t.Fatalf("expected num_ctx=8192, got %+v", req.Options)
		}
		if req.Options["temperature"] != float64(0) {
			t.Fatalf("expected temperature=0 for structured output, got %+v", req.Options)
		}
		if req.Think == nil || *req.Think {
			t.Fatalf("expected think=false in the request, got %+v", req.Think)
		}
		json.NewEncoder(w).Encode(wire{Response: `{"title":"ok"}`})
	}))
	defer srv.Close()
	got, err := New(srv.URL, "test-model", 5, 8192, false, "").Generate(context.Background(), "hello", schema)
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"title":"ok"}` {
		t.Fatalf("unexpected response: %q", got)
	}
}

// The ollama_think/ollama_keep_alive config lanes must reach the wire —
// callers pass them positionally, so this guards against a silent swap.
func TestGenerateSendsThinkAndKeepAlive(t *testing.T) {
	var got wire
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		json.NewEncoder(w).Encode(wire{Response: `{"title":"ok"}`})
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "model", 5, 8192, true, "5m").Generate(context.Background(), "p", json.RawMessage(`{"type":"object"}`)); err != nil {
		t.Fatal(err)
	}
	if got.Think == nil || !*got.Think {
		t.Fatalf("expected think=true in the request, got %+v", got.Think)
	}
	if got.KeepAlive != "5m0s" {
		t.Fatalf("expected keep_alive=5m0s (normalized duration), got %q", got.KeepAlive)
	}
}

func TestEmbedSendsKeepAlive(t *testing.T) {
	var keepAlive string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			KeepAlive string `json:"keep_alive"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		keepAlive = req.KeepAlive
		json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{0.1}}})
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "embed-model", 5, 0, false, "10m").Embed(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if keepAlive != "10m0s" {
		t.Fatalf("expected keep_alive=10m0s (normalized duration), got %q", keepAlive)
	}
}

func TestEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var req struct {
			Model    string `json:"model"`
			Input    string `json:"input"`
			Truncate bool   `json:"truncate"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Model != "embed-model" || req.Input != "hello" || !req.Truncate {
			t.Fatalf("unexpected request: %+v", req)
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{0.1, 0.2, 0.3}}})
	}))
	defer srv.Close()
	vec, err := New(srv.URL, "embed-model", 5, 0, false, "").Embed(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(vec) != 3 || vec[0] != 0.1 {
		t.Fatalf("unexpected vector: %v", vec)
	}
}

func TestGenerateHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) }))
	defer srv.Close()
	got, err := New(srv.URL, "x", 5, 0, false, "").Generate(context.Background(), "x", json.RawMessage(`"json"`))
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected an HTTP 500 error, got %v", err)
	}
	if got != "" {
		t.Fatalf("unexpected response body: %q", got)
	}
}

func TestGenerateSurfacesOllamaErrorField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(wire{Error: "model 'gone' not found"})
	}))
	defer srv.Close()
	_, err := New(srv.URL, "gone", 5, 0, false, "").Generate(context.Background(), "x", nil)
	if err == nil || !strings.Contains(err.Error(), "model 'gone' not found") {
		t.Fatalf("expected the ollama error field, got %v", err)
	}
}

func TestGenerateRejectsMalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()
	_, err := New(srv.URL, "x", 5, 0, false, "").Generate(context.Background(), "x", nil)
	if err == nil {
		t.Fatal("expected an error for a malformed response")
	}
}

func TestGenerateAutoNumCtxFloorsAt4096AndTrimsURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			http.Error(w, "not supported", 404) // no context_length -> fallback cap
			return
		}
		var req wire
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Options["num_ctx"] != float64(4096) {
			t.Fatalf("auto num_ctx should floor at 4096 for a tiny prompt, got %+v", req.Options)
		}
		json.NewEncoder(w).Encode(wire{Response: "ok"})
	}))
	defer srv.Close()
	// The trailing slash must be trimmed or the request would 404.
	got, err := New(srv.URL+"/", "m", 5, 0, false, "").Generate(context.Background(), "x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "ok" {
		t.Fatalf("response=%q", got)
	}
}

func TestGenerateAutoNumCtxScalesWithPromptAndCapsAtModelLimit(t *testing.T) {
	var shown int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/show":
			shown++
			json.NewEncoder(w).Encode(map[string]any{"model_info": map[string]any{"test.context_length": 8192}})
		case "/api/generate":
			var req wire
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			json.NewEncoder(w).Encode(wire{Response: fmt.Sprintf("%v", req.Options["num_ctx"])})
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "m", 5, 0, false, "")

	// 8 KB prompt -> 8192/4*2+1024 = 5120, under the model's 8192 limit.
	got, err := c.Generate(context.Background(), strings.Repeat("a", 8192), nil)
	if err != nil || got != "5120" {
		t.Fatalf("expected auto num_ctx 5120, got %q (err %v)", got, err)
	}
	// 20 KB prompt -> 11024 wanted, capped at the model's context_length.
	got, err = c.Generate(context.Background(), strings.Repeat("a", 20480), nil)
	if err != nil || got != "8192" {
		t.Fatalf("expected num_ctx capped at 8192, got %q (err %v)", got, err)
	}
	if shown != 1 {
		t.Fatalf("expected /api/show to be queried once and cached, got %d calls", shown)
	}
}

func TestGenerateAutoNumCtxFallsBackWhenShowFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			http.Error(w, "no", 500)
			return
		}
		var req wire
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		json.NewEncoder(w).Encode(wire{Response: fmt.Sprintf("%v", req.Options["num_ctx"])})
	}))
	defer srv.Close()
	// 32 KB prompt -> 17408 wanted, capped at the 16384 fallback.
	got, err := New(srv.URL, "m", 5, 0, false, "").Generate(context.Background(), strings.Repeat("a", 32768), nil)
	if err != nil || got != "16384" {
		t.Fatalf("expected num_ctx capped at the 16384 fallback, got %q (err %v)", got, err)
	}
}

func TestGenerateExplicitNumCtxSkipsShow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			t.Fatal("/api/show must not be queried when num_ctx is configured")
		}
		var req wire
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Options["num_ctx"] != float64(2048) {
			t.Fatalf("expected pinned num_ctx 2048, got %+v", req.Options)
		}
		json.NewEncoder(w).Encode(wire{Response: "ok"})
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "m", 5, 2048, false, "").Generate(context.Background(), "x", nil); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateHonoursRequestTimeout(t *testing.T) {
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-unblock // keep the response pending until the client timeout is asserted
	}))
	defer func() {
		close(unblock)
		srv.Close()
	}()
	_, err := New(srv.URL, "x", 1, 0, false, "").Generate(context.Background(), "x", nil)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
}
