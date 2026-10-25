package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSearchSendsBearerKeyAndParsesResults(t *testing.T) {
	var gotReq *http.Request
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotReq = r
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]string{
				{"title": "curl releases", "url": "https://curl.se", "content": "latest is 8.x"},
			},
		})
	}))
	defer srv.Close()

	results, err := New("test-key").WithBaseURL(srv.URL).Search(context.Background(), "curl latest release", 5)
	if err != nil {
		t.Fatal(err)
	}
	if gotReq.Method != http.MethodPost || gotReq.URL.Path != "/api/web_search" {
		t.Fatalf("unexpected request %s %s", gotReq.Method, gotReq.URL.Path)
	}
	if gotReq.Header.Get("Authorization") != "Bearer test-key" {
		t.Fatalf("missing bearer key: %q", gotReq.Header.Get("Authorization"))
	}
	var req struct {
		Query      string `json:"query"`
		MaxResults int    `json:"max_results"`
	}
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if req.Query != "curl latest release" || req.MaxResults != 5 {
		t.Fatalf("unexpected request body: %s", gotBody)
	}
	if len(results) != 1 || results[0].URL != "https://curl.se" || results[0].Content == "" {
		t.Fatalf("unexpected results: %+v", results)
	}
}

func TestSearchCapsRequestedResultCount(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]string{}})
	}))
	defer srv.Close()
	if _, err := New("k").WithBaseURL(srv.URL).Search(context.Background(), "q", 50); err != nil {
		t.Fatal(err)
	}
	var req struct {
		MaxResults int `json:"max_results"`
	}
	_ = json.Unmarshal(gotBody, &req)
	if req.MaxResults != 10 {
		t.Fatalf("max_results should be clamped to the server cap: %d", req.MaxResults)
	}
}

func TestSearchRejectsEmptyQueryAndKey(t *testing.T) {
	c := New("k")
	if _, err := c.Search(context.Background(), "   ", 5); err == nil {
		t.Fatal("empty query must fail")
	}
	if _, err := New("").Search(context.Background(), "q", 5); err == nil ||
		!strings.Contains(err.Error(), "OLLAMA_API_KEY") {
		t.Fatalf("missing key must fail closed before any request: %v", err)
	}
}

func TestSearchSurfacesServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid api key"})
	}))
	defer srv.Close()
	_, err := New("bad-key").WithBaseURL(srv.URL).Search(context.Background(), "q", 5)
	var statusErr StatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != 401 || !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("expected StatusError carrying the server message: %v", err)
	}
}

func TestSearchBlocksCrossOriginRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", http.StatusFound)
	}))
	defer srv.Close()
	if _, err := New("k").WithBaseURL(srv.URL).Search(context.Background(), "q", 5); err == nil ||
		!strings.Contains(err.Error(), "cross-origin") {
		t.Fatalf("cross-origin redirect must be blocked: %v", err)
	}
}
