package server

import (
	"context"
	"net/http"
	"slices"
	"time"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/ollama"
)

type modelOption struct {
	ollama.Model
	Configured bool `json:"configured"`
	Verified   bool `json:"verified"`
}

// The ordinary endpoint is offline. Discovery is an explicit, metadata-only
// request to the configured Ollama URL; it never pulls or loads models.
// Stamps carry the dominant recorded model per lineage column so the UI can
// mark "what ran last time" and warn before a different model requeues work.
func modelsData(c config.Config, db *database.DB) http.HandlerFunc {
	type taskInfo struct {
		Default string   `json:"default"`
		Options []string `json:"options"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		tasks := map[string]taskInfo{}
		for _, task := range []string{config.TaskEnrich, config.TaskAnalyze, config.TaskExpand, config.TaskFreshness, config.TaskVerify, config.TaskEmbed} {
			options := c.TaskModelOptions(task, false)
			info := taskInfo{Options: options}
			if len(options) > 0 {
				info.Default = options[0]
			}
			tasks[task] = info
		}
		var catalog []ollama.Model
		discovered, discoveryError := false, ""
		if r.URL.Query().Get("discover") == "1" {
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			var err error
			catalog, err = ollama.New(c.OllamaURL, "", 5, 0, false, "").DiscoverModels(ctx)
			if err != nil {
				discoveryError = err.Error()
			} else {
				discovered = true
			}
		}
		unverified := []modelOption{}
		for _, model := range catalog {
			if !model.Supports(ollama.CapabilityCompletion) && !model.Supports(ollama.CapabilityEmbedding) {
				unverified = append(unverified, modelOption{Model: model, Configured: c.ValidModel(model.Name)})
			}
		}
		stamps, err := db.ModelStamps(r.Context())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"models":            c.AllModels(),
			"stamps":            stamps,
			"external_default":  c.OllamaModelExternal,
			"tasks":             tasks,
			"generation_models": modelOptions(c, c.GenerationModels(), catalog, ollama.CapabilityCompletion, discovered),
			"embedding_models":  modelOptions(c, c.EmbeddingModels(), catalog, ollama.CapabilityEmbedding, discovered),
			"unverified_models": unverified,
			"discovered":        discovered,
			"discovery_error":   discoveryError,
		})
	}
}

func modelOptions(c config.Config, configured []string, catalog []ollama.Model, capability string, discovered bool) []modelOption {
	out := []modelOption{}
	for _, name := range configured {
		idx := slices.IndexFunc(catalog, func(m ollama.Model) bool {
			return ollama.CanonicalModelName(m.Name) == ollama.CanonicalModelName(name)
		})
		option := modelOption{
			Model:      ollama.Model{Name: name, Capabilities: []string{}, Locality: "unknown"},
			Configured: true,
		}
		if idx >= 0 {
			option.Model = catalog[idx]
			option.Name = name
			option.Verified = option.Supports(capability) && option.Error == ""
			if len(option.Capabilities) > 0 && !option.Supports(capability) {
				continue
			}
		} else {
			switch {
			case name == c.OllamaModelExternal || ollama.CloudName(name):
				option.Locality = "cloud"
			case ollama.RemoteServer(c.OllamaURL):
				option.Locality = "remote"
			}
			if discovered {
				option.Error = "Not installed/registered on the configured Ollama server"
			}
		}
		out = append(out, option)
	}
	for _, model := range catalog {
		if !model.Supports(capability) || slices.ContainsFunc(out, func(option modelOption) bool {
			return ollama.CanonicalModelName(option.Name) == ollama.CanonicalModelName(model.Name)
		}) {
			continue
		}
		out = append(out, modelOption{Model: model, Verified: true})
	}
	return out
}
