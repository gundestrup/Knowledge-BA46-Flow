package modelselection

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/config"
)

func modelServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var response map[string]any
		switch req.Model {
		case "local-gen", "base-local", "fast-local":
			response = map[string]any{
				"capabilities": []string{"completion"},
				"model_info":   map[string]any{"test.context_length": 8192},
			}
		case "local-embed":
			response = map[string]any{
				"capabilities": []string{"embedding"},
				"model_info":   map[string]any{"test.context_length": 2048},
			}
		case "cloud-gen:cloud":
			response = map[string]any{
				"capabilities": []string{"completion"},
				"remote_host":  "ollama.com",
			}
		case "cloud-embed:cloud":
			response = map[string]any{
				"capabilities": []string{"embedding"},
				"remote_host":  "ollama.com",
			}
		default:
			response = map[string]any{
				"capabilities": []string{},
				"model_info":   map[string]any{"test.context_length": 8192},
			}
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
}

func baseConfig(serverURL string) config.Config {
	return config.Config{
		OllamaURL:           serverURL,
		OllamaModel:         "base-local",
		OllamaModelFast:     "fast-local",
		OllamaModelExternal: "cloud-gen:cloud",
		OllamaEmbedModel:    "local-embed",
	}
}

func TestPrepareSkipsDiscoveryForOperationsWithoutModelOptions(t *testing.T) {
	showCalls := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			showCalls <- struct{}{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"capabilities": []string{"completion"},
			"model_info":   map[string]any{"test.context_length": 8192},
		})
	}))
	defer server.Close()
	c := baseConfig(server.URL)
	got, err := Prepare(context.Background(), c, "scan", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.OllamaTaskModels) != 0 {
		t.Fatalf("no-op model preparation changed config: %+v", got.OllamaTaskModels)
	}
	select {
	case <-showCalls:
		t.Fatal("non-LLM operation must not contact Ollama")
	default:
	}
}

func TestPrepareAddsVerifiedLocalGenerationModelToCopy(t *testing.T) {
	server := modelServer(t)
	defer server.Close()
	c := baseConfig(server.URL)
	got, err := Prepare(context.Background(), c, "analyze", Options{Model: "local-gen"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.OllamaTaskModels) != 0 {
		t.Fatalf("Prepare mutated the caller's config: %+v", c.OllamaTaskModels)
	}
	if model, err := got.TaskModel(config.TaskAnalyze, false, "local-gen"); err != nil || model != "local-gen" {
		t.Fatalf("verified model not available in returned config: model=%q err=%v", model, err)
	}
}

func TestPrepareRequiresExplicitConsentForCloudGeneration(t *testing.T) {
	server := modelServer(t)
	defer server.Close()
	c := baseConfig(server.URL)

	if _, err := Prepare(context.Background(), c, "analyze", Options{Model: "cloud-gen:cloud"}); err == nil ||
		!strings.Contains(err.Error(), "explicit cloud/remote consent") {
		t.Fatalf("cloud generation should require explicit consent, got %v", err)
	}
	if _, err := Prepare(context.Background(), c, "analyze", Options{
		Model: "cloud-gen:cloud", External: true,
	}); err != nil {
		t.Fatalf("explicit external consent should permit the configured cloud model: %v", err)
	}
}

func TestPrepareRequiresExternalForCloudTaskDefault(t *testing.T) {
	server := modelServer(t)
	defer server.Close()
	c := baseConfig(server.URL)

	_, err := Prepare(context.Background(), c, "expand", Options{})
	if err == nil || !strings.Contains(err.Error(), "requires explicit remote consent") {
		t.Fatalf("cloud task default should be blocked without remote consent, got %v", err)
	}
	if _, err := Prepare(context.Background(), c, "expand", Options{External: true}); err != nil {
		t.Fatalf("explicit external flag should permit cloud task default: %v", err)
	}
}

func TestPrepareAllowRemoteConsentsWithoutRerouting(t *testing.T) {
	server := modelServer(t)
	defer server.Close()
	c := baseConfig(server.URL)

	// The configured expand default resolves to the cloud model via the lane
	// fallback; --allow-remote consents without switching to the external lane.
	got, err := Prepare(context.Background(), c, "all", Options{AllowRemote: true})
	if err != nil {
		t.Fatalf("--allow-remote should permit remote resolved models: %v", err)
	}
	if model, err := got.TaskModel(config.TaskAnalyze, false, ""); err != nil || model != "base-local" {
		t.Fatalf("analyze routing must stay on its local default, got %q err %v", model, err)
	}
	if model, err := got.TaskModel(config.TaskExpand, false, ""); err != nil || model != "cloud-gen:cloud" {
		t.Fatalf("expand must keep its configured cloud default, got %q err %v", model, err)
	}
}

func TestPrepareAllowRemotePermitsCloudOverrides(t *testing.T) {
	server := modelServer(t)
	defer server.Close()
	c := baseConfig(server.URL)

	if _, err := Prepare(context.Background(), c, "analyze", Options{
		Model: "cloud-gen:cloud", AllowRemote: true,
	}); err != nil {
		t.Fatalf("--allow-remote should consent to a cloud generation override: %v", err)
	}
	if _, err := Prepare(context.Background(), c, "embed", Options{
		Model: "cloud-embed:cloud", AllowRemote: true,
	}); err != nil {
		t.Fatalf("--allow-remote should consent to a cloud embedding override: %v", err)
	}
}

func TestPrepareKeepsGenerationAndEmbeddingCapabilitiesSeparate(t *testing.T) {
	server := modelServer(t)
	defer server.Close()
	c := baseConfig(server.URL)

	if _, err := Prepare(context.Background(), c, "embed", Options{Model: "local-gen"}); err == nil ||
		!strings.Contains(err.Error(), "does not advertise the \"embedding\" capability") {
		t.Fatalf("generation model must not be accepted for embedding: %v", err)
	}
	if _, err := Prepare(context.Background(), c, "embed", Options{Model: "local-embed"}); err != nil {
		t.Fatalf("verified embedding model should be accepted: %v", err)
	}
}

func TestPrepareRequiresAllowCloudForCloudEmbedding(t *testing.T) {
	server := modelServer(t)
	defer server.Close()
	c := baseConfig(server.URL)

	if _, err := Prepare(context.Background(), c, "embed", Options{Model: "cloud-embed:cloud"}); err == nil ||
		!strings.Contains(err.Error(), "explicit cloud/remote consent") {
		t.Fatalf("cloud embedding should require consent: %v", err)
	}
	if _, err := Prepare(context.Background(), c, "embed", Options{
		Model: "cloud-embed:cloud", AllowCloud: true,
	}); err != nil {
		t.Fatalf("--allow-cloud should permit cloud embeddings: %v", err)
	}
}

func TestPrepareRejectsUnsupportedOptions(t *testing.T) {
	server := modelServer(t)
	defer server.Close()
	c := baseConfig(server.URL)

	if _, err := Prepare(context.Background(), c, "scan", Options{Model: "local-gen"}); err == nil {
		t.Fatal("scan must reject model options")
	}
	if _, err := Prepare(context.Background(), c, "enrich", Options{External: true}); err == nil {
		t.Fatal("enrichment must use --allow-cloud rather than --external")
	}
}

func TestPrepareValidatesVerifyLaneConsent(t *testing.T) {
	server := modelServer(t)
	defer server.Close()
	c := baseConfig(server.URL)
	c.OllamaModelExternal = "" // freshness resolves to local base-local
	c.OllamaTaskModels = map[string][]string{config.TaskVerify: {"cloud-gen:cloud"}}

	if _, err := Prepare(context.Background(), c, "freshness", Options{WebVerify: true}); err == nil ||
		!strings.Contains(err.Error(), "requires explicit remote consent") {
		t.Fatalf("a cloud verify default must demand consent, got %v", err)
	}
	// The same remote verify lane is not validated when verification is not
	// requested — the unused option cannot demand consent by itself.
	c.OllamaTaskModels[config.TaskVerify] = []string{"base-local"}
	if _, err := Prepare(context.Background(), c, "freshness", Options{WebVerify: true}); err != nil {
		t.Fatalf("a local verify lane should pass: %v", err)
	}
}

func TestPrepareRejectsWebVerifyOnOtherOps(t *testing.T) {
	server := modelServer(t)
	defer server.Close()
	c := baseConfig(server.URL)
	if _, err := Prepare(context.Background(), c, "expand", Options{WebVerify: true}); err == nil ||
		!strings.Contains(err.Error(), "does not support web verification") {
		t.Fatalf("web_verify on a non-freshness op must be rejected, got %v", err)
	}
}
