package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/ollama"
)

type selectionCalls struct {
	mu         sync.Mutex
	metadata   int
	generation []string
	embedding  []string
}

func selectionAPI(t *testing.T) (config.Config, *database.DB, *httptest.Server, *selectionCalls) {
	t.Helper()
	c, db := taskOpsFixture(t, `{}`)
	c.OllamaEmbedModel = "configured-embed"
	c.OllamaTaskModels = map[string][]string{config.TaskEmbed: {"task-embed", "configured-embed"}}
	capabilities := map[string][]string{
		"test-model": {"completion"}, "found-chat": {"completion"},
		"configured-embed": {"embedding"}, "task-embed": {"embedding"},
		"found-embed": {"embedding"}, "remote-embed": {"embedding"}, "unknown": {},
	}
	calls := &selectionCalls{}
	models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model  string `json:"model"`
			Prompt string `json:"prompt"`
			Input  string `json:"input"`
		}
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("invalid model request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		calls.mu.Lock()
		defer calls.mu.Unlock()
		switch r.URL.Path {
		case "/api/tags":
			calls.metadata++
			items := []ollama.Model{}
			for name := range capabilities {
				items = append(items, ollama.Model{Name: name})
			}
			json.NewEncoder(w).Encode(map[string]any{"models": items})
		case "/api/show":
			calls.metadata++
			caps, exists := capabilities[req.Model]
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			reply := map[string]any{
				"capabilities": caps, "model_info": map[string]any{"test.context_length": 8192},
			}
			if req.Model == "remote-embed" {
				reply["remote_host"] = "https://example.invalid"
			}
			json.NewEncoder(w).Encode(reply)
		case "/api/generate":
			if req.Prompt != "" {
				calls.generation = append(calls.generation, req.Model)
			}
			json.NewEncoder(w).Encode(map[string]string{"response": `{"title":"Doc","summary":"s","tags":["tool"],"patch":null,"obsolete":false,"current":true,"flags":[],"suggestions":[]}`})
		case "/api/embed":
			calls.embedding = append(calls.embedding, req.Model)
			json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{1, 0}}})
		default:
			t.Errorf("unexpected model endpoint, possibly a download: %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(models.Close)
	c.OllamaURL = models.URL
	app := httptest.NewServer(NewMux(c, db, nil))
	t.Cleanup(app.Close)
	return c, db, app, calls
}

func TestModelsAPISeparatesCapabilitiesAndDiscoversUnconfiguredModels(t *testing.T) {
	_, _, app, calls := selectionAPI(t)
	var response struct {
		Generation []modelOption `json:"generation_models"`
		Embedding  []modelOption `json:"embedding_models"`
		Unverified []modelOption `json:"unverified_models"`
		Discovered bool          `json:"discovered"`
	}
	getJSON(t, app.URL+"/api/models", &response)
	calls.mu.Lock()
	count := calls.metadata
	calls.mu.Unlock()
	if count != 0 || response.Discovered {
		t.Fatal("ordinary model-list request must stay offline")
	}
	getJSON(t, app.URL+"/api/models?discover=1", &response)
	if !response.Discovered {
		t.Fatal("explicit discovery did not complete")
	}
	for _, model := range response.Generation {
		if !model.Supports("completion") || model.Supports("embedding") {
			t.Fatalf("wrong generation choice: %+v", model)
		}
	}
	for _, model := range response.Embedding {
		if !model.Supports("embedding") || model.Supports("completion") {
			t.Fatalf("wrong embedding choice: %+v", model)
		}
	}
	if !slices.ContainsFunc(response.Generation, func(m modelOption) bool { return m.Name == "found-chat" && !m.Configured && m.Verified }) ||
		!slices.ContainsFunc(response.Embedding, func(m modelOption) bool { return m.Name == "found-embed" && !m.Configured && m.Verified }) ||
		!slices.ContainsFunc(response.Embedding, func(m modelOption) bool { return m.Name == "remote-embed" && m.Locality == "cloud" }) ||
		len(response.Unverified) != 1 || response.Unverified[0].Name != "unknown" {
		t.Fatalf("discovered choices=%+v", response)
	}
	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.generation) != 0 || len(calls.embedding) != 0 {
		t.Fatal("discovery loaded a model or sent document content")
	}
}

func TestOperationsAcceptIndependentUnconfiguredModelsWithoutSavingDefaults(t *testing.T) {
	for _, op := range []string{"run", "all"} {
		t.Run(op, func(t *testing.T) {
			c, db, app, calls := selectionAPI(t)
			started := postSelectionJob(t, app.URL, `{"op":"`+op+`","model":"found-chat","embed_model":"found-embed","reanalyze":true}`)
			finished := waitSelectionJob(t, app.URL, started.ID)
			if finished.Status != "done" {
				t.Fatalf("job failed: %+v", finished)
			}
			calls.mu.Lock()
			if len(calls.generation) == 0 || len(calls.embedding) == 0 {
				t.Errorf("both selected roles must run: generation=%v embedding=%v", calls.generation, calls.embedding)
			}
			for _, name := range calls.generation {
				if name != "found-chat" {
					t.Errorf("generation received wrong model %q", name)
				}
			}
			for _, name := range calls.embedding {
				if name != "found-embed" {
					t.Errorf("embedding received wrong model %q", name)
				}
			}
			calls.mu.Unlock()
			docs, err := db.Documents(t.Context())
			if err != nil || docs[0].EmbeddedModel != "found-embed" || docs[0].AnalyzedModel != "found-chat" {
				t.Fatalf("wrong role lineage: %+v error=%v", docs, err)
			}
			var configured struct{ Models []string }
			getJSON(t, app.URL+"/api/models", &configured)
			if slices.Contains(configured.Models, "found-chat") || slices.Contains(configured.Models, "found-embed") ||
				c.OllamaEmbedModel != "configured-embed" {
				t.Fatal("per-run selections changed the server's defaults")
			}
		})
	}
}

func TestOperationsRejectWrongCapabilitiesAndCloudEmbeddingWithoutConsent(t *testing.T) {
	_, db, app, calls := selectionAPI(t)
	for _, body := range []string{
		`{"op":"run","model":"found-embed"}`,
		`{"op":"run","embed_model":"found-chat"}`,
		`{"op":"run","embed_model":"remote-embed"}`,
		`{"op":"embed","model":"found-chat","embed_model":"found-embed"}`,
	} {
		if status := postStatus(t, http.MethodPost, app.URL+"/api/ops", body); status != http.StatusBadRequest {
			t.Fatalf("unsafe request accepted: %s status=%d", body, status)
		}
	}
	jobs, err := db.Jobs(t.Context(), 10)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("invalid selections created jobs: %+v error=%v", jobs, err)
	}
	calls.mu.Lock()
	defer calls.mu.Unlock()
	if len(calls.generation) != 0 || len(calls.embedding) != 0 {
		t.Fatal("rejected selections sent document content")
	}
}

func TestCloudEmbeddingConsentDoesNotChangeLocalGenerationLane(t *testing.T) {
	c, db, app, _ := selectionAPI(t)
	original, err := os.ReadFile(filepath.Join(c.RepositoryPath, "a.md"))
	if err != nil {
		t.Fatal(err)
	}
	started := postSelectionJob(t, app.URL, `{"op":"run","model":"found-chat","embed_model":"remote-embed","allow_cloud":true,"reanalyze":true}`)
	if job := waitSelectionJob(t, app.URL, started.ID); job.Status != "done" {
		t.Fatalf("consented cloud embedding failed: %+v", job)
	}
	docs, err := db.Documents(t.Context())
	if err != nil || docs[0].EmbeddedModel != "remote-embed" || docs[0].AnalyzedModel != "found-chat" || docs[0].ExternalModel != "" {
		t.Fatalf("embedding consent changed generation provenance: %+v error=%v", docs, err)
	}
	after, err := os.ReadFile(filepath.Join(c.RepositoryPath, "a.md"))
	if err != nil || string(original) != string(after) {
		t.Fatal("run applied unaccepted suggestions")
	}
}

func postSelectionJob(t *testing.T, baseURL, body string) database.Job {
	t.Helper()
	response, err := http.Post(baseURL+"/api/ops", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		var failure any
		json.NewDecoder(response.Body).Decode(&failure)
		t.Fatalf("operation status=%d error=%+v", response.StatusCode, failure)
	}
	var job database.Job
	if err := json.NewDecoder(response.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	return job
}

func waitSelectionJob(t *testing.T, baseURL string, id int64) database.Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var job database.Job
		getJSON(t, baseURL+"/api/ops/"+strconv.FormatInt(id, 10), &job)
		if job.Status != "running" {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("model-selection job did not finish")
	return database.Job{}
}
