package ollama

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetadataOutputLimitAndContextBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			json.NewEncoder(w).Encode(map[string]any{"model_info": map[string]any{"test.context_length": 16384}})
			return
		}
		var req wire
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		// A 16 KB prompt is estimated at 4096 tokens. Reserve only 512
		// output tokens plus JSON headroom, not another document-sized reply.
		if req.Options["num_predict"] != float64(512) || req.Options["num_ctx"] != float64(5632) {
			t.Errorf("unbounded metadata request: %+v", req.Options)
		}
		if req.Think == nil || *req.Think {
			t.Error("metadata generation must send think:false")
		}
		json.NewEncoder(w).Encode(wire{Response: `{"title":"ok"}`})
	}))
	defer srv.Close()
	client := New(srv.URL, "fast", 5, 0, false, "").WithOutputLimit(512)
	if _, err := client.Generate(t.Context(), strings.Repeat("x", 16384), json.RawMessage(`{"type":"object"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataOutputCutoffIsNotASuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"response": `{"title":"cut off`, "done": true, "done_reason": "length",
		})
	}))
	defer srv.Close()
	client := New(srv.URL, "fast", 5, 4096, false, "").WithOutputLimit(64)
	if _, err := client.Generate(t.Context(), "metadata", nil); err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("cutoff must fail rather than create a partial proposal: %v", err)
	}
}

func TestUnloadMissingOptionalModelIsIdempotent(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				json.NewEncoder(w).Encode(wire{Error: "unavailable"})
			}))
			defer srv.Close()
			err := New(srv.URL, "fast", 5, 4096, false, "").Unload(t.Context())
			if (err == nil) != (status == http.StatusNotFound) {
				t.Fatalf("unload status=%d error=%v", status, err)
			}
		})
	}
}
