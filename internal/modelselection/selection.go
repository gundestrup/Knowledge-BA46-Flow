// Package modelselection validates per-run choices shared by the CLI and API.
// Discovered models are admitted only to an isolated in-memory config copy.
package modelselection

import (
	"context"
	"errors"
	"fmt"
	"time"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/ollama"
)

type Options struct {
	Model      string
	EmbedModel string
	External   bool
	AllowCloud bool
}

func (o Options) EmbeddingOverride() string {
	if o.EmbedModel != "" {
		return o.EmbedModel
	}
	return o.Model
}

func Prepare(ctx context.Context, c config.Config, op string, opts Options) (config.Config, error) {
	tasks := generationTasks(op)
	skip, err := validateOptions(c, op, tasks, opts)
	if err != nil || skip {
		return c, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := ollama.New(c.OllamaURL, "", 5, 0, false, "")
	if err := verifyOverrides(ctx, client, &c, op, opts); err != nil {
		return c, err
	}
	if err := validateConfiguredTasks(c, tasks, opts); err != nil {
		return c, err
	}
	if err := validateEmbeddingRoute(c, op, opts); err != nil {
		return c, err
	}
	return c, nil
}

func validateOptions(c config.Config, op string, tasks []string, opts Options) (bool, error) {
	if len(tasks) == 0 && op != "embed" {
		if opts != (Options{}) {
			return false, fmt.Errorf("op %q does not accept model options", op)
		}
		return true, nil
	}
	if opts.External && (op == "embed" || op == "enrich") {
		return false, fmt.Errorf("op %q does not support the external analysis lane; use --allow-cloud for cloud embeddings/enrichment", op)
	}
	if opts.EmbedModel != "" && op != "analyze" && op != "run" && op != "all" && op != "embed" {
		return false, fmt.Errorf("op %q does not use an embedding model", op)
	}
	if op == "embed" && opts.Model != "" && opts.EmbedModel != "" {
		return false, errors.New("use either --model or --embed-model for embed, not both")
	}
	if opts.External && opts.Model == "" && c.OllamaModelExternal == "" {
		return false, errors.New("--external requires ollama_model_external or an explicit generation --model")
	}
	return false, nil
}

func verifyOverrides(ctx context.Context, client *ollama.Client, c *config.Config, op string, opts Options) error {
	if op != "embed" && opts.Model != "" {
		allowRemote := opts.External || (op == "enrich" && opts.AllowCloud)
		if err := verifyModel(ctx, client, c, opts.Model, config.TaskAnalyze, ollama.CapabilityCompletion, allowRemote); err != nil {
			return err
		}
	}
	embedOverride := opts.EmbedModel
	if op == "embed" {
		embedOverride = opts.EmbeddingOverride()
	}
	if embedOverride != "" {
		if err := verifyModel(ctx, client, c, embedOverride, config.TaskEmbed, ollama.CapabilityEmbedding, opts.AllowCloud); err != nil {
			return err
		}
	}
	return nil
}

func verifyModel(ctx context.Context, client *ollama.Client, c *config.Config, name, task, capability string, allowRemote bool) error {
	info, err := client.ShowModel(ctx, name)
	if err == nil {
		err = info.Validate(capability, allowRemote)
	}
	if err != nil {
		return fmt.Errorf("verify %s model %q: %w (use --external for cloud generation, --allow-cloud for cloud embeddings/enrichment)", task, name, err)
	}
	*c = c.WithModelOption(task, name)
	return nil
}

func validateConfiguredTasks(c config.Config, tasks []string, opts Options) error {
	for _, task := range tasks {
		model, err := c.TaskModel(task, opts.External, opts.Model)
		if err != nil {
			return err
		}
		knownRemote := knownRemoteModel(c, model)
		allowed := opts.External || (task == config.TaskEnrich && opts.AllowCloud)
		if knownRemote && !allowed {
			return fmt.Errorf("model %q requires explicit --external consent (--allow-cloud for enrichment only)", model)
		}
	}
	return nil
}

func validateEmbeddingRoute(c config.Config, op string, opts Options) error {
	if op == "embed" || op == "analyze" || op == "run" || op == "all" {
		embedOverride := opts.EmbedModel
		if op == "embed" {
			embedOverride = opts.EmbeddingOverride()
		}
		model, err := c.TaskModel(config.TaskEmbed, false, embedOverride)
		if err != nil && (!errors.Is(err, config.ErrNoTaskModel) || op == "embed") {
			return err
		}
		knownRemote := model != "" && knownRemoteModel(c, model)
		if knownRemote && !opts.AllowCloud {
			return fmt.Errorf("embedding model %q requires explicit --allow-cloud consent", model)
		}
	}
	return nil
}

func knownRemoteModel(c config.Config, model string) bool {
	return model == c.OllamaModelExternal || ollama.CloudName(model) ||
		(c.OllamaURL != "" && ollama.RemoteServer(c.OllamaURL))
}

func generationTasks(op string) []string {
	switch op {
	case "all":
		return []string{config.TaskEnrich, config.TaskExpand, config.TaskFreshness, config.TaskAnalyze}
	case "analyze", "run":
		return []string{config.TaskEnrich, config.TaskAnalyze}
	case "enrich", "expand", "freshness":
		return []string{op}
	default:
		return nil
	}
}
