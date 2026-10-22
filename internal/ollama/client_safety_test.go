package ollama

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGenerateRejectsTruncatedResponseEvenWhenJSONParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"response": `{"suggestions":[]}`, "done": true, "done_reason": "length",
		})
	}))
	defer srv.Close()
	got, err := New(srv.URL, "model", 5, 8192, false, "").Generate(t.Context(), "prompt", json.RawMessage(`{"type":"object"}`))
	if err == nil || !strings.Contains(err.Error(), "truncated") || got != "" {
		t.Fatalf("response=%q error=%v", got, err)
	}
}

func TestUnloadDoesNotSendDocumentOrLoadContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/generate" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var req struct {
			Model     string         `json:"model"`
			Prompt    string         `json:"prompt"`
			KeepAlive string         `json:"keep_alive"`
			Options   map[string]any `json:"options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Model != "model" || req.Prompt != "" || req.KeepAlive != "0s" || len(req.Options) != 0 {
			t.Fatalf("unload payload=%+v", req)
		}
		json.NewEncoder(w).Encode(map[string]any{"done": true})
	}))
	defer srv.Close()
	if err := New(srv.URL, "model", 5, 0, false, "").Unload(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateBlocksCrossOriginRedirect(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		json.NewEncoder(w).Encode(map[string]any{"response": `{"unexpected":true}`})
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/api/generate", http.StatusTemporaryRedirect)
	}))
	defer source.Close()

	_, err := New(source.URL, "model", 5, 4096, false, "").Generate(t.Context(), "private prompt", json.RawMessage(`{"type":"object"}`))
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("expected cross-origin redirect error, got %v", err)
	}
	if got := redirected.Load(); got != 0 {
		t.Fatalf("redirect target received %d request(s)", got)
	}
}
