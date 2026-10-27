// Package config loads and validates the JSON configuration file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"knowledge-ba46-flow/internal/paths"
)

// Config mirrors the JSON config file. Path fields are resolved to absolute
// paths in Load; fields left empty get the defaults applied below.
type Config struct {
	RepositoryPath      string       `json:"repository_path"`       // root of the markdown knowledge repository under maintenance
	DatabasePath        string       `json:"database_path"`         // SQLite file tracking documents and suggestions
	PromptDirectory     string       `json:"prompt_directory"`      // directory containing base.md and analyze.md
	Report              ReportConfig `json:"report"`                // optional local repository health report
	OllamaURL           string       `json:"ollama_url"`            // base URL of the Ollama server, e.g. http://localhost:11434
	OllamaModel         string       `json:"ollama_model"`          // model name sent to /api/generate for full analysis
	OllamaModelFast     string       `json:"ollama_model_fast"`     // optional cheaper model for the metadata/enrichment pass; default = ollama_model
	OllamaModelExternal string       `json:"ollama_model_external"` // optional enhanced analysis model for explicit external runs (e.g. a cloud model); results stamp the external lane, leaving the local analyzed_* lineage untouched
	OllamaEmbedModel    string       `json:"ollama_embed_model"`    // optional embedding model for semantic candidate ranking (e.g. embeddinggemma:300m); empty = shingle-only ranking
	// OllamaTaskModels lists per-task model choices, first entry being the
	// default and the rest UI-selectable alternatives (e.g.
	// {"expand": ["gpt-oss:120b-cloud", "gemma4:31b"]}). Known tasks:
	// enrich, analyze, expand, freshness, verify, embed. "verify" is the
	// optional second pass of freshness --web-verify — the model that
	// re-judges flagged claims against web search evidence, separate from
	// the model that raised the flags. A lane field is the
	// fallback when a task has no entry.
	OllamaTaskModels         map[string][]string `json:"ollama_task_models"`
	OllamaNumCtx             int                 `json:"ollama_num_ctx"`             // context window sent as options.num_ctx; 0/unset = auto-size per request from prompt + model context_length, >0 = fixed override
	OllamaThink              bool                `json:"ollama_think"`               // let reasoning models think before answering; default false — thinking buys nothing for strict JSON output and multiplies latency
	OllamaKeepAlive          string              `json:"ollama_keep_alive"`          // how long Ollama keeps a model loaded after each request (e.g. "5m", "0" to unload immediately); empty = Ollama default
	RequestTimeoutSeconds    int                 `json:"request_timeout_seconds"`    // per-request HTTP timeout; default 300
	EnrichmentTimeoutSeconds int                 `json:"enrichment_timeout_seconds"` // separate optional-stage deadline; default 120
	EnrichmentMaxTokens      int                 `json:"enrichment_max_tokens"`      // maximum enrichment output tokens; default 512
	EnrichmentMode           string              `json:"enrichment_mode"`            // local-first derives missing title/tags from trusted H1/path evidence before optional fast generation
	StubMaxBodyLines         int                 `json:"stub_max_body_lines"`        // documents with at most this many non-empty body lines are placeholder stubs; unset/0 = 5, negative disables size detection (the `stub` tag always applies)
	MaxDocumentBytes         int64               `json:"max_document_bytes"`         // documents larger than this are skipped; default 200000
	Extensions               []string            `json:"extensions"`                 // file extensions to scan; default [.md .markdown]
	ExcludeDirectories       []string            `json:"exclude_directories"`        // directory names skipped during the walk (match is by name only)
	Exclude                  []string            `json:"exclude"`                    // repository-root-anchored paths/patterns skipped during the walk; '*', '?' and '[...]' work per segment, a whole '**' segment matches any depth
	ExcludeExtensions        []string            `json:"exclude_extensions"`         // extensions removed from the extensions allowlist (case-insensitive, leading '.' required)
	TrashDirectory           string              `json:"trash_directory"`            // deletions are moved here, inside the repository; default .kbm-trash
	WriteScope               string              `json:"write_scope"`                // metadata-links (default), content, or all; checked during generation, review and apply
	HTTPAddr                 string              `json:"http_addr"`                  // listen address for `KBM-flow serve`; default 127.0.0.1:8080
	LogFile                  string              `json:"log_file"`                   // optional: also write logs to this file (appended); relative paths resolve against the config dir
	LogLevel                 string              `json:"log_level"`                  // "info" (default) or "debug" — debug adds per-request HTTP lines and job lifecycle detail
	// ReindexTriggerPath names a repository-relative file touched after a
	// successful apply batch. Point the Knowledge Base MCP Server's
	// REINDEX_TRIGGER_PATH at the same file so applied changes trigger a
	// reindex automatically. Empty disables the touch; the value must stay
	// inside the repository (validated like a document path) so the touch
	// remains within the os.Root confinement.
	ReindexTriggerPath string `json:"reindex_trigger_path"`
	// MCPServer names the knowledge-base MCP server the corpus is curated for.
	// It selects the prompt file prompts/<mcp_server>.md (frontmatter taxonomy
	// and conventions). Only MCPServerKnowledgeBase is currently supported.
	MCPServer string `json:"mcp_server"`
	// Validate configures the read-only `KBM-flow validate` structural gate.
	Validate ValidateConfig `json:"validate"`
	// OllamaAPIKey is the bearer key for Ollama's hosted web search
	// (POST https://ollama.com/api/web_search), used by `freshness
	// --web-verify`. It is env/.env only — json:"-" keeps the secret out of
	// config.json and out of the committed file format entirely.
	OllamaAPIKey string `json:"-"`
}

// ValidateConfig scopes the structural validation rules in internal/validate.
// Exempt paths are outside knowledge scope: no frontmatter/tag requirement, no
// index back-link requirement, and their non-Markdown siblings need no links.
// Patterns use the same root-anchored syntax as `exclude`.
type ValidateConfig struct {
	Exempt []string `json:"exempt"`
}

const (
	EnrichmentModeLocalFirst = "local-first"
	EnrichmentModeAlways     = "always"
)

// FastModel returns the model used for the cheap enrichment pass (title,
// summary, tags, frontmatter patch). Without an explicit ollama_model_fast it
// falls back to OllamaModel, which disables the separate pass — a single
// analysis call then covers both jobs.
func (c Config) FastModel() string {
	if c.OllamaModelFast != "" {
		return c.OllamaModelFast
	}
	return c.OllamaModel
}

// AnalysisModel resolves which model the analysis pass uses for a run:
// the external model when an external run is requested, otherwise the
// configured ollama_model. Empty OllamaModelExternal for an external run is
// a configuration error handled by the caller.
func (c Config) AnalysisModel(external bool) string {
	if external {
		return c.OllamaModelExternal
	}
	return c.OllamaModel
}

// LLM task names — also the job op names, suggestion origins and
// stage_results stages for the optional passes.
const (
	TaskEnrich    = "enrich"
	TaskAnalyze   = "analyze"
	TaskExpand    = "expand"
	TaskFreshness = "freshness"
	TaskEmbed     = "embed"
	// TaskVerify is the optional evidence pass of `freshness --web-verify`:
	// it re-judges flagged claims against web search results. It is never a
	// standalone op, but its model routes like every other task.
	TaskVerify = "verify"
)

// llmTasks is the closed set ollama_task_models may configure.
var llmTasks = []string{TaskEnrich, TaskAnalyze, TaskExpand, TaskFreshness, TaskVerify, TaskEmbed}

// ErrNoTaskModel marks "this task lane has no configured model" — batch runs
// ("all" op) skip such tasks instead of failing.
var ErrNoTaskModel = errors.New("no model configured for task")

// TaskModel resolves which model runs a task: an explicit per-run override
// (which must name a configured model) wins, then the task's configured
// default, then the lane fallback. The external flag forces the external lane
// for the tasks that support it (analyze, expand, freshness) and errors when
// no external model is configured.
func (c Config) TaskModel(task string, external bool, override string) (string, error) {
	if !slices.Contains(llmTasks, task) {
		return "", fmt.Errorf("unknown task %q", task)
	}
	if override != "" {
		options := c.GenerationModels()
		if task == TaskEmbed {
			options = c.EmbeddingModels()
		}
		if !slices.Contains(options, override) {
			return "", fmt.Errorf("model %q is not configured for task %q", override, task)
		}
		return override, nil
	}
	laneCapable := task == TaskAnalyze || task == TaskExpand || task == TaskFreshness || task == TaskVerify
	if external && laneCapable {
		if c.OllamaModelExternal == "" {
			return "", fmt.Errorf("%s with --external requires ollama_model_external in the config", task)
		}
		return c.OllamaModelExternal, nil
	}
	// An explicit per-task default beats the lane fallback.
	if list := c.OllamaTaskModels[task]; len(list) > 0 && list[0] != "" {
		return list[0], nil
	}
	var m string
	switch task {
	case TaskEnrich:
		m = c.FastModel()
	case TaskAnalyze:
		m = c.OllamaModel
	case TaskExpand, TaskFreshness, TaskVerify:
		// These enhanced tasks ride the external lane when one is
		// configured — they are exactly what a stronger model is for.
		m = c.OllamaModelExternal
		if m == "" {
			m = c.OllamaModel
		}
	case TaskEmbed:
		m = c.OllamaEmbedModel
	default:
		return "", fmt.Errorf("unknown task %q", task)
	}
	if m == "" {
		return "", fmt.Errorf("%w %q (see ollama_model*, ollama_embed_model, ollama_task_models)", ErrNoTaskModel, task)
	}
	return m, nil
}

// VerifyModel resolves which model the freshness verification pass runs:
// the same routing as every task (per-run override → --external →
// ollama_task_models.verify → external lane → ollama_model). fallback is the
// freshness model, retained when resolution fails — verify is a sub-pass,
// so a lane hiccup must not lose the primary check.
func (c Config) VerifyModel(external bool, override, fallback string) string {
	if m, err := c.TaskModel(TaskVerify, external, override); err == nil {
		return m
	}
	return fallback
}

// TaskModelOptions lists the models a task may run with, default first: the
// resolved default, the configured task alternatives, then the lane fallback
// when it differs — every entry is a valid per-run override.
func (c Config) TaskModelOptions(task string, external bool) []string {
	def, _ := c.TaskModel(task, external, "")
	out := []string{}
	if def != "" {
		out = append(out, def)
	}
	for _, m := range c.OllamaTaskModels[task] {
		if m != "" && !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	// The lane fallback stays selectable even when a task default overrides it.
	var lane string
	switch task {
	case TaskEnrich:
		lane = c.FastModel()
	case TaskAnalyze:
		lane = c.OllamaModel
	case TaskEmbed:
		lane = c.OllamaEmbedModel
	default:
		lane = c.OllamaModel
	}
	if lane != "" && !slices.Contains(out, lane) {
		out = append(out, lane)
	}
	return out
}

// ValidModel reports whether name is one of the configured models and may be
// used as a per-run override.
func (c Config) ValidModel(name string) bool {
	return slices.Contains(c.AllModels(), name)
}

// AllModels returns every configured model name — the union a per-run
// override may pick from.
func (c Config) AllModels() []string {
	out := []string{}
	for _, m := range []string{c.OllamaModel, c.OllamaModelFast, c.OllamaModelExternal, c.OllamaEmbedModel} {
		if m != "" && !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	for _, task := range llmTasks {
		for _, m := range c.OllamaTaskModels[task] {
			if m != "" && !slices.Contains(out, m) {
				out = append(out, m)
			}
		}
	}
	return out
}

// GenerationModels and EmbeddingModels keep configured choices separated by
// role. Live Ollama capability checks still validate explicit run selections.
func (c Config) GenerationModels() []string {
	out := []string{}
	for _, m := range []string{c.OllamaModel, c.FastModel(), c.OllamaModelExternal} {
		if m != "" && !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	for _, task := range llmTasks {
		if task == TaskEmbed {
			continue
		}
		for _, m := range c.OllamaTaskModels[task] {
			if !slices.Contains(out, m) {
				out = append(out, m)
			}
		}
	}
	return out
}

func (c Config) EmbeddingModels() []string {
	out := append([]string{}, c.OllamaTaskModels[TaskEmbed]...)
	if c.OllamaEmbedModel != "" && !slices.Contains(out, c.OllamaEmbedModel) {
		out = append(out, c.OllamaEmbedModel)
	}
	return out
}

// WithModelOption admits a verified, per-run choice without changing shared
// configuration or its backing slices. It never writes the config file.
func (c Config) WithModelOption(task, model string) Config {
	c.OllamaTaskModels = maps.Clone(c.OllamaTaskModels)
	if c.OllamaTaskModels == nil {
		c.OllamaTaskModels = map[string][]string{}
	}
	if !slices.Contains(c.OllamaTaskModels[task], model) {
		c.OllamaTaskModels[task] = append(slices.Clone(c.OllamaTaskModels[task]), model)
	}
	return c
}

// StubLineLimit supplies the default for programmatic configurations too:
// unset/zero means 5 non-empty body lines; a negative value disables
// size-based stub detection so only the explicit `stub` tag applies.
func (c Config) StubLineLimit() int {
	if c.StubMaxBodyLines == 0 {
		return 5
	}
	return c.StubMaxBodyLines
}

// EnrichmentTimeout also supplies the default for programmatic configurations.
func (c Config) EnrichmentTimeout() time.Duration {
	seconds := c.EnrichmentTimeoutSeconds
	if seconds <= 0 {
		seconds = 120
	}
	return time.Duration(seconds) * time.Second
}

// EnrichmentOutputTokens bounds metadata generation independently of analysis.
func (c Config) EnrichmentOutputTokens() int {
	if c.EnrichmentMaxTokens > 0 {
		return c.EnrichmentMaxTokens
	}
	return 512
}

// MCPServerKnowledgeBase is the only supported value of mcp_server: the
// markdown conventions and the prompts/<name>.md prompt file target the
// github.com/jeanibarz/knowledge-base-mcp-server frontmatter taxonomy.
const MCPServerKnowledgeBase = "knowledge-base-mcp-server"

// ReportConfig points to the read-only registry/policy and optional comparison
// snapshot used by the repository report. Registry paths inside repos.yml are
// resolved relative to that file, not to the application config.
type ReportConfig struct {
	RegistryPath   string `json:"registry_path"`
	PolicyPath     string `json:"policy_path"`
	ComparisonPath string `json:"comparison_path"`
	RemoteEnabled  *bool  `json:"remote_enabled,omitempty"`
}

// WantsRemote defaults to false for programmatic/test configs. Load enables
// remote checks by default when a registry is configured.
func (r ReportConfig) WantsRemote() bool {
	return r.RemoteEnabled != nil && *r.RemoteEnabled
}

// Load reads the JSON config, validates required fields and fills in defaults.
// Relative paths are resolved against the directory containing the config
// file, so the same config works from any working directory.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("parse config: %w", err)
	}
	applyEnvOverrides(&c, loadDotEnv(filepath.Dir(path)))
	if c.RepositoryPath == "" || c.DatabasePath == "" || c.PromptDirectory == "" || c.OllamaURL == "" || c.OllamaModel == "" {
		return c, fmt.Errorf("config requires repository_path, database_path, prompt_directory, ollama_url and ollama_model")
	}
	if c.RequestTimeoutSeconds <= 0 {
		c.RequestTimeoutSeconds = 300 // LLM calls are slow; a generous default
	}
	if c.EnrichmentTimeoutSeconds == 0 {
		c.EnrichmentTimeoutSeconds = int(c.EnrichmentTimeout().Seconds())
	}
	if c.EnrichmentMaxTokens == 0 {
		c.EnrichmentMaxTokens = c.EnrichmentOutputTokens()
	}
	if c.EnrichmentMode == "" {
		c.EnrichmentMode = EnrichmentModeLocalFirst
	}
	if c.MaxDocumentBytes <= 0 {
		c.MaxDocumentBytes = 200000 // cap prompt size sent to Ollama
	}
	if len(c.Extensions) == 0 {
		c.Extensions = []string{".md", ".markdown"}
	}
	if c.TrashDirectory == "" {
		c.TrashDirectory = ".kbm-trash"
	}
	c.WriteScope = c.EffectiveWriteScope()
	if c.HTTPAddr == "" {
		// Localhost-only default: /api/apply mutates the filesystem, so binding
		// wider than loopback must be a deliberate choice.
		c.HTTPAddr = "127.0.0.1:8080"
	}
	if c.MCPServer == "" {
		c.MCPServer = MCPServerKnowledgeBase
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if err := validateOptionalFields(&c); err != nil {
		return c, err
	}
	c.HTTPAddr, err = normalizeHTTPAddr(c.HTTPAddr)
	if err != nil {
		return c, err
	}
	resolveRelativePaths(&c, path)
	if c.Report.RegistryPath != "" && c.Report.RemoteEnabled == nil {
		remote := true
		c.Report.RemoteEnabled = &remote
	}
	return c, nil
}

// resolveRelativePaths anchors every path field to the config file's
// directory, so the same config works from any working directory.
func resolveRelativePaths(c *Config, path string) {
	base, _ := filepath.Abs(filepath.Dir(path))
	c.RepositoryPath = joinAbs(base, c.RepositoryPath)
	c.DatabasePath = joinAbs(base, c.DatabasePath)
	c.PromptDirectory = joinAbs(base, c.PromptDirectory)
	if c.LogFile != "" {
		c.LogFile = joinAbs(base, c.LogFile)
	}
	if c.Report.RegistryPath != "" {
		c.Report.RegistryPath = joinAbs(base, c.Report.RegistryPath)
	}
	if c.Report.PolicyPath != "" {
		c.Report.PolicyPath = joinAbs(base, c.Report.PolicyPath)
	}
	if c.Report.ComparisonPath != "" {
		c.Report.ComparisonPath = joinAbs(base, c.Report.ComparisonPath)
	}
}

// validateOptionalFields checks the fields that need value validation rather
// than a default, after Load has applied defaults.
func validateOptionalFields(c *Config) error {
	switch c.EffectiveWriteScope() {
	case "metadata-links", "content", "all":
	default:
		return fmt.Errorf("invalid write_scope %q: use metadata-links, content or all", c.WriteScope)
	}
	if c.EnrichmentTimeoutSeconds < 0 || c.EnrichmentMaxTokens < 0 {
		return fmt.Errorf("enrichment_timeout_seconds and enrichment_max_tokens must be positive, or 0 for defaults")
	}
	if c.EnrichmentMode != EnrichmentModeLocalFirst && c.EnrichmentMode != EnrichmentModeAlways {
		return fmt.Errorf("invalid enrichment_mode %q: use %q or %q", c.EnrichmentMode, EnrichmentModeLocalFirst, EnrichmentModeAlways)
	}
	if c.OllamaNumCtx < 0 {
		return fmt.Errorf("invalid ollama_num_ctx %d: use 0 for automatic sizing or a positive token count", c.OllamaNumCtx)
	}
	if c.MCPServer != MCPServerKnowledgeBase {
		return fmt.Errorf("invalid mcp_server %q: only %q is supported", c.MCPServer, MCPServerKnowledgeBase)
	}
	if c.LogLevel != "info" && c.LogLevel != "debug" {
		return fmt.Errorf("invalid log_level %q: use \"info\" or \"debug\"", c.LogLevel)
	}
	if c.OllamaKeepAlive != "" {
		if _, err := time.ParseDuration(c.OllamaKeepAlive); err != nil {
			return fmt.Errorf("invalid ollama_keep_alive %q: expected a duration like 5m or 30s (or 0 to unload immediately)", c.OllamaKeepAlive)
		}
	}
	if c.ReindexTriggerPath != "" {
		if _, err := paths.CleanRelative(c.ReindexTriggerPath); err != nil {
			return fmt.Errorf("reindex_trigger_path must stay inside the repository: %w", err)
		}
	}
	for task, models := range c.OllamaTaskModels {
		if !slices.Contains(llmTasks, task) {
			return fmt.Errorf("invalid ollama_task_models task %q: use one of %s", task, strings.Join(llmTasks, ", "))
		}
		if len(models) == 0 {
			return fmt.Errorf("ollama_task_models.%s must list at least one model", task)
		}
		for _, m := range models {
			if strings.TrimSpace(m) == "" {
				return fmt.Errorf("ollama_task_models.%s contains an empty model name", task)
			}
		}
	}
	if _, err := paths.ParseExcludePatterns(c.Exclude); err != nil {
		return err
	}
	if _, err := paths.ParseExcludePatterns(c.Validate.Exempt); err != nil {
		return fmt.Errorf("validate.exempt: %w", err)
	}
	return validateExcludeExtensions(c)
}

// EffectiveWriteScope keeps zero-value configs as restrictive as loaded configs.
func (c Config) EffectiveWriteScope() string {
	if c.WriteScope == "" {
		return "metadata-links"
	}
	return c.WriteScope
}

// validateExcludeExtensions checks the exclude_extensions entries and that
// they do not empty the effective scan set. Entries use the same form as
// extensions ("." prefix required); comparison is case-insensitive.
func validateExcludeExtensions(c *Config) error {
	exts := map[string]bool{}
	for _, e := range c.Extensions {
		exts[strings.ToLower(e)] = true
	}
	for _, e := range c.ExcludeExtensions {
		if !strings.HasPrefix(e, ".") || strings.ContainsAny(e, "/\\") {
			return fmt.Errorf("invalid exclude_extensions entry %q: expected a file extension like .csv", e)
		}
		delete(exts, strings.ToLower(e))
	}
	if len(exts) == 0 {
		return fmt.Errorf("exclude_extensions removes every extension in 'extensions' — nothing would be scanned")
	}
	return nil
}

// applyEnvOverrides fills deployment-specific fields from the environment so
// a shared compose .env can drive both this program and the MCP server.
// OLLAMA_BASE_URL is deliberately the MCP server's own variable name: one
// value reaches both containers. Overrides run before required-field
// validation, so env alone can satisfy them. A real environment variable
// always wins over a .env file entry.
func applyEnvOverrides(c *Config, dotenv map[string]string) {
	lookup := func(env string) string {
		if v := os.Getenv(env); v != "" {
			return v
		}
		return dotenv[env]
	}
	for _, o := range []struct {
		env string
		dst *string
	}{
		{"OLLAMA_BASE_URL", &c.OllamaURL},
		{"OLLAMA_API_KEY", &c.OllamaAPIKey},
		{"KBM_REPOSITORY_PATH", &c.RepositoryPath},
		{"KBM_DATABASE_PATH", &c.DatabasePath},
		{"KBM_HTTP_ADDR", &c.HTTPAddr},
		{"KBM_OLLAMA_MODEL", &c.OllamaModel},
		{"KBM_OLLAMA_MODEL_FAST", &c.OllamaModelFast},
		{"KBM_OLLAMA_MODEL_EXTERNAL", &c.OllamaModelExternal},
		{"KBM_ENRICHMENT_MODE", &c.EnrichmentMode},
		{"KBM_LOG_FILE", &c.LogFile},
		{"KBM_LOG_LEVEL", &c.LogLevel},
	} {
		if v := lookup(o.env); v != "" {
			*o.dst = v
		}
	}
}

// loadDotEnv reads KEY=VALUE lines from a .env file beside the JSON config —
// the same file docker-compose interpolates, honored for direct runs too so
// secrets like OLLAMA_API_KEY never land in the committed config. A missing
// or unreadable file is not an error; the real environment may carry the
// values instead.
func loadDotEnv(dir string) map[string]string {
	b, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if key != "" {
			out[key] = value
		}
	}
	return out
}

// normalizeHTTPAddr accepts host:port or a bare port (localhost only).
// An empty host such as ":8080" implicitly binds all interfaces, so require
// callers to opt into that explicitly with "0.0.0.0:8080".
func normalizeHTTPAddr(addr string) (string, error) {
	if _, err := strconv.Atoi(addr); err == nil {
		addr = net.JoinHostPort("127.0.0.1", addr)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("invalid http_addr %q: expected host:port or a bare port", addr)
	}
	if host == "" {
		return "", fmt.Errorf("invalid http_addr %q: specify a host explicitly (use 0.0.0.0 to bind all interfaces)", addr)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("invalid http_addr %q: port must be between 1 and 65535", addr)
	}
	return net.JoinHostPort(host, strconv.Itoa(n)), nil
}

// joinAbs returns p if it is already absolute, otherwise base/p resolved to an
// absolute path.
func joinAbs(base, p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	abs, err := filepath.Abs(filepath.Join(base, p))
	if err != nil {
		return filepath.Join(base, p)
	}
	return abs
}
