package ollama

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestDiscoverModelsUsesOnlyMetadataAndKeepsUnknownsUnverified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			json.NewEncoder(w).Encode(map[string]any{"models": []Model{
				{Name: "chat:latest", Digest: "chat-digest"}, {Name: "embed:4b"},
				{Name: "cloud-alias"}, {Name: "unknown"}, {Name: "broken"}, {Name: "chat:latest"},
			}})
			return
		}
		if r.URL.Path != "/api/show" || r.Method != http.MethodPost {
			t.Errorf("discovery attempted a non-metadata request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req struct{ Model string }
		json.NewDecoder(r.Body).Decode(&req)
		reply := map[string]any{"model_info": map[string]any{"test.context_length": 8192}}
		switch req.Model {
		case "chat:latest":
			reply["capabilities"] = []string{"completion", "tools"}
		case "embed:4b":
			reply["capabilities"] = []string{"embedding"}
		case "cloud-alias":
			reply["capabilities"] = []string{"completion"}
			reply["remote_host"] = "https://example.invalid"
		case "unknown":
			reply = map[string]any{}
		case "broken":
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "missing metadata"})
			return
		}
		json.NewEncoder(w).Encode(reply)
	}))
	defer srv.Close()
	models, err := New(srv.URL, "", 2, 0, false, "").DiscoverModels(t.Context())
	if err != nil || len(models) != 5 {
		t.Fatalf("models=%+v error=%v", models, err)
	}
	byName := map[string]Model{}
	for _, model := range models {
		byName[model.Name] = model
	}
	if !byName["chat:latest"].Supports(CapabilityCompletion) || byName["chat:latest"].Digest != "chat-digest" ||
		byName["chat:latest"].Locality != "local" || byName["chat:latest"].ContextLength != 8192 {
		t.Fatalf("chat=%+v", byName["chat:latest"])
	}
	if !byName["embed:4b"].Supports(CapabilityEmbedding) || byName["embed:4b"].Supports(CapabilityCompletion) {
		t.Fatalf("embed=%+v", byName["embed:4b"])
	}
	if byName["cloud-alias"].Locality != "cloud" || byName["unknown"].Locality != "unknown" ||
		len(byName["unknown"].Capabilities) != 0 || byName["broken"].Error == "" {
		t.Fatalf("unverified/locality classifications=%+v", models)
	}
}

func TestModelChecksRunBeforeAnyDocumentContentIsSent(t *testing.T) {
	for _, tc := range []struct {
		name        string
		capability  string
		remote      bool
		metadata    bool
		allowRemote bool
		wantError   bool
	}{
		{name: "local generation", capability: "completion", metadata: true},
		{name: "local embedding", capability: "embedding", metadata: true},
		{name: "wrong capability", capability: "vision", metadata: true, wantError: true},
		{name: "cloud alias blocked", capability: "completion", metadata: true, remote: true, wantError: true},
		{name: "cloud embedding blocked", capability: "embedding", metadata: true, remote: true, wantError: true},
		{name: "cloud embedding consented", capability: "embedding", metadata: true, remote: true, allowRemote: true},
		{name: "unknown locality blocked", capability: "completion", wantError: true},
		{name: "unknown locality consented", capability: "completion", allowRemote: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var contentCalls, metadataCalls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/show" {
					metadataCalls.Add(1)
					reply := map[string]any{"capabilities": []string{tc.capability}}
					if tc.metadata {
						reply["model_info"] = map[string]any{"test.context_length": 8192}
					}
					if tc.remote {
						reply["remote_model"] = "remote-provider-model"
					}
					json.NewEncoder(w).Encode(reply)
					return
				}
				contentCalls.Add(1)
				if r.URL.Path == "/api/embed" {
					json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{1, 0}}})
				} else {
					json.NewEncoder(w).Encode(map[string]string{"response": "{}"})
				}
			}))
			defer srv.Close()
			required := CapabilityCompletion
			if tc.capability == CapabilityEmbedding {
				required = CapabilityEmbedding
			}
			client := New(srv.URL, "alias", 2, 8192, false, "").WithModelCheck(required, tc.allowRemote)
			var err error
			for range 2 {
				if required == CapabilityEmbedding {
					_, err = client.Embed(t.Context(), "private document text")
				} else {
					_, err = client.Generate(t.Context(), "private document text", nil)
				}
			}
			if (err != nil) != tc.wantError || metadataCalls.Load() != 1 {
				t.Fatalf("error=%v metadata calls=%d", err, metadataCalls.Load())
			}
			wantContent := int32(2)
			if tc.wantError {
				wantContent = 0
			}
			if contentCalls.Load() != wantContent {
				t.Fatalf("sent content %d times; want %d", contentCalls.Load(), wantContent)
			}
		})
	}
}

func TestMetadataFailuresAndModelNames(t *testing.T) {
	for _, tc := range []struct {
		body string
		code int
	}{
		{body: `{"error":"server failure"}`, code: 500},
		{body: `{"error":"metadata error"}`, code: 200},
		{body: `{`, code: 200},
	} {
		t.Run(tc.body, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			client := New(srv.URL, "", 2, 0, false, "")
			if _, err := client.ListModels(t.Context()); err == nil {
				t.Fatal("invalid model list was accepted")
			}
			if _, err := client.ShowModel(t.Context(), "model"); err == nil {
				t.Fatal("invalid model metadata was accepted")
			}
		})
	}
	if got := CanonicalModelName("registry:1234/model"); got != "registry:1234/model:latest" {
		t.Fatalf("canonical name=%q", got)
	}
	if !reflect.DeepEqual([]bool{RemoteServer("http://127.0.0.1:1"), RemoteServer("http://[::1]:1"), RemoteServer("http://localhost:1")}, []bool{false, false, false}) {
		t.Fatal("loopback was classified as remote")
	}
	for _, host := range []string{"http://192.168.1.2:11434", "https://ollama.com", "http://localhost.evil.invalid", ""} {
		if !RemoteServer(host) {
			t.Errorf("remote URL %q classified as local", host)
		}
	}
	if !CloudName("gemma:31b-cloud") || CloudName("embeddinggemma:300m") {
		t.Fatal("conservative cloud-name guard is incorrect")
	}
	if CanonicalModelName("model") != "model:latest" {
		t.Fatal("unexpected canonical model name")
	}
}
