package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"knowledge-ba46-flow/internal/database"
)

func TestCLIUsesIndependentGenerationAndEmbeddingOverrides(t *testing.T) {
	for _, command := range []string{"analyze", "run", "tasks"} {
		t.Run(command, func(t *testing.T) {
			dir := t.TempDir()
			repo, prompts := filepath.Join(dir, "repo"), filepath.Join(dir, "prompts")
			if err := os.MkdirAll(repo, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(prompts, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, "guide.md"),
				[]byte("---\ntags: [tool]\n---\n# Guide\n\nSubstantial guide content.\n"), 0644); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"base", "knowledge-base-mcp-server", "analyze", "enrich", "expand", "freshness"} {
				if err := os.WriteFile(filepath.Join(prompts, name+".md"), []byte(name), 0644); err != nil {
					t.Fatal(err)
				}
			}
			var mu sync.Mutex
			generation, embedding := []string{}, []string{}
			models := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Model  string `json:"model"`
					Prompt string `json:"prompt"`
				}
				json.NewDecoder(r.Body).Decode(&req)
				switch r.URL.Path {
				case "/api/show":
					capability := "completion"
					if req.Model == "chosen-embed" {
						capability = "embedding"
					}
					json.NewEncoder(w).Encode(map[string]any{
						"capabilities": []string{capability}, "model_info": map[string]int{"test.context_length": 8192},
					})
				case "/api/generate":
					if req.Prompt != "" {
						mu.Lock()
						generation = append(generation, req.Model)
						mu.Unlock()
					}
					json.NewEncoder(w).Encode(map[string]string{"response": `{"title":"Guide","summary":"s","tags":["tool"],"obsolete":false,"current":true,"flags":[],"suggestions":[]}`})
				case "/api/embed":
					mu.Lock()
					embedding = append(embedding, req.Model)
					mu.Unlock()
					json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{1, 0}}})
				default:
					t.Errorf("unexpected model endpoint: %s", r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer models.Close()
			path := writeCLIConfig(t, dir, map[string]any{
				"ollama_url": models.URL, "ollama_embed_model": "old-embed",
				"ollama_task_models":  map[string][]string{"embed": {"task-embed"}},
				"stub_max_body_lines": -1,
			})
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if command != "run" {
				if err := runCLI("scan", "--config", path); err != nil {
					t.Fatal(err)
				}
			}
			if err := runCLI(command, "--config", path, "--model", "chosen-chat", "--embed-model", "chosen-embed"); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			if len(generation) == 0 || len(embedding) == 0 {
				t.Errorf("both selected models must run: generation=%v embedding=%v", generation, embedding)
			}
			for _, name := range generation {
				if name != "chosen-chat" {
					t.Errorf("generation used %q", name)
				}
			}
			for _, name := range embedding {
				if name != "chosen-embed" {
					t.Errorf("embedding used %q", name)
				}
			}
			mu.Unlock()
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatal("per-run CLI selections rewrote the configuration")
			}
			db, err := database.Open(t.Context(), filepath.Join(dir, "data", "knowledge.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			docs, err := db.Documents(t.Context())
			if err != nil || len(docs) != 1 || docs[0].EmbeddedModel != "chosen-embed" || docs[0].AnalyzedModel != "chosen-chat" {
				t.Fatalf("wrong CLI lineage: %+v error=%v", docs, err)
			}
		})
	}
}

func TestEmbeddingSelectionFlagsAreAvailableOnRelevantCommands(t *testing.T) {
	for _, name := range []string{"analyze", "run", "tasks", "embed"} {
		command, _, err := rootCmd().Find([]string{name})
		if err != nil || command.Flags().Lookup("embed-model") == nil || command.Flags().Lookup("allow-cloud") == nil {
			t.Fatalf("%s is missing independent embedding flags: %v", name, err)
		}
	}
}
