package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, values map[string]any) string {
	t.Helper()
	data, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func requiredConfig() map[string]any {
	return map[string]any{
		"repository_path":  "repo",
		"database_path":    "data/km.db",
		"prompt_directory": "prompts",
		"ollama_url":       "http://localhost:11434",
		"ollama_model":     "test",
	}
}

func TestLoadDefaultsAndAbsolutePaths(t *testing.T) {
	path := writeConfig(t, requiredConfig())
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Dir(path)
	if c.RepositoryPath != filepath.Join(configDir, "repo") ||
		c.DatabasePath != filepath.Join(configDir, "data", "km.db") ||
		c.PromptDirectory != filepath.Join(configDir, "prompts") {
		t.Fatalf("relative paths were not resolved from config directory: %+v", c)
	}
	if c.RequestTimeoutSeconds != 300 || c.OllamaNumCtx != 0 || c.MaxDocumentBytes != 200000 {
		t.Fatalf("unexpected numeric defaults: %+v", c)
	}
	if c.HTTPAddr != "127.0.0.1:8080" || c.TrashDirectory != ".kbm-trash" {
		t.Fatalf("unexpected HTTP/trash defaults: %+v", c)
	}
	if c.EnrichmentMode != EnrichmentModeLocalFirst {
		t.Fatalf("enrichment_mode=%q, want local-first", c.EnrichmentMode)
	}
	if len(c.Extensions) != 2 || c.Extensions[0] != ".md" || c.Extensions[1] != ".markdown" {
		t.Fatalf("unexpected extension defaults: %v", c.Extensions)
	}
}

func TestEnrichmentModeValidation(t *testing.T) {
	for _, mode := range []string{EnrichmentModeLocalFirst, EnrichmentModeAlways} {
		values := requiredConfig()
		values["enrichment_mode"] = mode
		if _, err := Load(writeConfig(t, values)); err != nil {
			t.Errorf("valid enrichment_mode %q rejected: %v", mode, err)
		}
	}
	values := requiredConfig()
	values["enrichment_mode"] = "sometimes"
	if _, err := Load(writeConfig(t, values)); err == nil || !strings.Contains(err.Error(), "enrichment_mode") {
		t.Fatalf("invalid enrichment_mode was accepted: %v", err)
	}
}

func TestLogLevelAndLogFile(t *testing.T) {
	c, err := Load(writeConfig(t, requiredConfig()))
	if err != nil {
		t.Fatal(err)
	}
	if c.LogLevel != "info" || c.LogFile != "" {
		t.Fatalf("unexpected log defaults: level=%q file=%q", c.LogLevel, c.LogFile)
	}
	for _, level := range []string{"info", "debug"} {
		values := requiredConfig()
		values["log_level"] = level
		if _, err := Load(writeConfig(t, values)); err != nil {
			t.Errorf("valid log_level %q rejected: %v", level, err)
		}
	}
	values := requiredConfig()
	values["log_level"] = "trace"
	if _, err := Load(writeConfig(t, values)); err == nil || !strings.Contains(err.Error(), "log_level") {
		t.Fatalf("invalid log_level was accepted: %v", err)
	}
	values = requiredConfig()
	values["log_file"] = "logs/kbm.log"
	path := writeConfig(t, values)
	c, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(filepath.Dir(path), "logs", "kbm.log"); c.LogFile != want {
		t.Fatalf("log_file=%q, want %q", c.LogFile, want)
	}
}

func TestOllamaTuningFields(t *testing.T) {
	values := requiredConfig()
	values["ollama_think"] = true
	values["ollama_keep_alive"] = "30s"
	c, err := Load(writeConfig(t, values))
	if err != nil {
		t.Fatal(err)
	}
	if !c.OllamaThink || c.OllamaKeepAlive != "30s" {
		t.Fatalf("think=%v keep_alive=%q", c.OllamaThink, c.OllamaKeepAlive)
	}
	for _, valid := range []string{"0", "5m", "1h30m"} {
		values := requiredConfig()
		values["ollama_keep_alive"] = valid
		if _, err := Load(writeConfig(t, values)); err != nil {
			t.Errorf("valid ollama_keep_alive %q rejected: %v", valid, err)
		}
	}
	values = requiredConfig()
	values["ollama_keep_alive"] = "soon"
	if _, err := Load(writeConfig(t, values)); err == nil || !strings.Contains(err.Error(), "ollama_keep_alive") {
		t.Fatalf("invalid ollama_keep_alive was accepted: %v", err)
	}
}

func TestLoadResolvesRelativeToConfigDir(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "cfg")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(configDir, "config.json")
	data, err := json.Marshal(requiredConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(configDir, "repo"); c.RepositoryPath != want {
		t.Fatalf("repository_path=%q want %q", c.RepositoryPath, want)
	}
}

func TestLoadResolvesReportSourcePaths(t *testing.T) {
	values := requiredConfig()
	values["report"] = map[string]any{
		"registry_path":   "../engineering-memory/repos.yml",
		"policy_path":     "../engineering-memory/policy.yml",
		"comparison_path": "../engineering-memory/report/data.json",
	}
	path := writeConfig(t, values)

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Dir(path)
	if want := filepath.Join(base, "../engineering-memory/repos.yml"); c.Report.RegistryPath != want {
		t.Fatalf("report.registry_path=%q want %q", c.Report.RegistryPath, want)
	}
	if want := filepath.Join(base, "../engineering-memory/policy.yml"); c.Report.PolicyPath != want {
		t.Fatalf("report.policy_path=%q want %q", c.Report.PolicyPath, want)
	}
	if want := filepath.Join(base, "../engineering-memory/report/data.json"); c.Report.ComparisonPath != want {
		t.Fatalf("report.comparison_path=%q want %q", c.Report.ComparisonPath, want)
	}
	if !c.Report.WantsRemote() {
		t.Fatal("remote report checks should default on when a registry is configured")
	}
}

func TestLoadCanDisableRemoteReportChecks(t *testing.T) {
	values := requiredConfig()
	values["report"] = map[string]any{
		"registry_path":  "repos.yml",
		"remote_enabled": false,
	}
	c, err := Load(writeConfig(t, values))
	if err != nil {
		t.Fatal(err)
	}
	if c.Report.WantsRemote() {
		t.Fatal("remote report checks remained enabled")
	}
}

func TestLoadNormalizesHTTPAddr(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want string
	}{
		{name: "bare port defaults to loopback", addr: "9090", want: "127.0.0.1:9090"},
		{name: "explicit host", addr: "0.0.0.0:8081", want: "0.0.0.0:8081"},
		{name: "IPv6", addr: "[::1]:8082", want: "[::1]:8082"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values := requiredConfig()
			values["http_addr"] = tt.addr
			c, err := Load(writeConfig(t, values))
			if err != nil {
				t.Fatal(err)
			}
			if c.HTTPAddr != tt.want {
				t.Fatalf("http_addr=%q want %q", c.HTTPAddr, tt.want)
			}
		})
	}
}

func TestLoadRejectsUnsafeOrInvalidHTTPAddr(t *testing.T) {
	for _, addr := range []string{":8080", "localhost", "127.0.0.1:0", "127.0.0.1:70000"} {
		t.Run(addr, func(t *testing.T) {
			values := requiredConfig()
			values["http_addr"] = addr
			if _, err := Load(writeConfig(t, values)); err == nil {
				t.Fatalf("expected invalid http_addr %q to be rejected", addr)
			}
		})
	}
}

func TestLoadRejectsIncompleteConfig(t *testing.T) {
	path := writeConfig(t, map[string]any{"repository_path": "repo"})
	if _, err := Load(path); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestLoadRejectsUnreadableAndMalformedConfig(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("expected an error for a missing config file")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for malformed JSON")
	}
}

func TestLoadKeepsAbsolutePaths(t *testing.T) {
	repo := t.TempDir()
	db := filepath.Join(t.TempDir(), "km.db")
	prompts := t.TempDir()
	values := requiredConfig()
	values["repository_path"] = repo
	values["database_path"] = db
	values["prompt_directory"] = prompts
	c, err := Load(writeConfig(t, values))
	if err != nil {
		t.Fatal(err)
	}
	if c.RepositoryPath != repo || c.DatabasePath != db || c.PromptDirectory != prompts {
		t.Fatalf("absolute paths were rewritten: %+v", c)
	}
}

func TestMCPServerDefaultsAndRejectsUnknown(t *testing.T) {
	// Unset -> the supported default.
	path := writeConfig(t, requiredConfig())
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.MCPServer != MCPServerKnowledgeBase {
		t.Fatalf("mcp_server=%q", c.MCPServer)
	}
	// Explicit supported value accepted.
	values := requiredConfig()
	values["mcp_server"] = "knowledge-base-mcp-server"
	if _, err := Load(writeConfig(t, values)); err != nil {
		t.Fatal(err)
	}
	// Anything else is rejected.
	values["mcp_server"] = "other-server"
	if _, err := Load(writeConfig(t, values)); err == nil ||
		!strings.Contains(err.Error(), "mcp_server") {
		t.Fatalf("expected mcp_server rejection, got %v", err)
	}
}

func TestLoadEnvironmentOverrides(t *testing.T) {
	// Env wins over the JSON file and can satisfy required fields — a shared
	// compose .env feeds both this program and the MCP server.
	t.Setenv("OLLAMA_BASE_URL", "http://ollama:11434")
	t.Setenv("KBM_REPOSITORY_PATH", "/knowledge/shelf")
	t.Setenv("KBM_DATABASE_PATH", "/data/kbm.db")
	t.Setenv("KBM_HTTP_ADDR", "0.0.0.0:8080")
	t.Setenv("KBM_ENRICHMENT_MODE", EnrichmentModeAlways)
	path := writeConfig(t, map[string]any{
		"repository_path":  "repo",
		"database_path":    "data/km.db",
		"prompt_directory": "prompts",
		"ollama_url":       "http://ignored.example",
		"ollama_model":     "test",
		"http_addr":        "127.0.0.1:9999",
	})
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.OllamaURL != "http://ollama:11434" ||
		c.RepositoryPath != "/knowledge/shelf" ||
		c.DatabasePath != "/data/kbm.db" ||
		c.HTTPAddr != "0.0.0.0:8080" ||
		c.EnrichmentMode != EnrichmentModeAlways {
		t.Fatalf("env overrides not applied: %+v", c)
	}
}

func TestLoadRejectsTriggerPathEscapes(t *testing.T) {
	for _, bad := range []string{"../outside", "/abs/path", "a/../../b"} {
		values := requiredConfig()
		values["reindex_trigger_path"] = bad
		if _, err := Load(writeConfig(t, values)); err == nil {
			t.Fatalf("reindex_trigger_path %q should be rejected", bad)
		}
	}
	values := requiredConfig()
	values["reindex_trigger_path"] = ".reindex-trigger"
	c, err := Load(writeConfig(t, values))
	if err != nil {
		t.Fatal(err)
	}
	if c.ReindexTriggerPath != ".reindex-trigger" {
		t.Fatalf("reindex_trigger_path=%q", c.ReindexTriggerPath)
	}
}

func TestLoadRejectsInvalidExcludePatterns(t *testing.T) {
	for _, bad := range []string{"/abs/dir", "../escape", "a/../b", "a//b", "a**b", "[", "  "} {
		values := requiredConfig()
		values["exclude"] = []string{bad}
		if _, err := Load(writeConfig(t, values)); err == nil {
			t.Fatalf("exclude pattern %q should be rejected", bad)
		}
	}
	values := requiredConfig()
	values["exclude"] = []string{"docs/tmp", "drafts/**", "**/*.draft.md"}
	if _, err := Load(writeConfig(t, values)); err != nil {
		t.Fatalf("valid exclude patterns rejected: %v", err)
	}
}

func TestLoadRejectsInvalidOrExhaustiveExcludeExtensions(t *testing.T) {
	values := requiredConfig()
	values["extensions"] = []string{".md", ".csv"}
	values["exclude_extensions"] = []string{".csv"}
	if _, err := Load(writeConfig(t, values)); err != nil {
		t.Fatalf("valid exclude_extensions rejected: %v", err)
	}
	for _, bad := range []string{"csv", "", "a/b"} {
		values["exclude_extensions"] = []string{bad}
		if _, err := Load(writeConfig(t, values)); err == nil {
			t.Fatalf("exclude_extensions entry %q should be rejected", bad)
		}
	}
	values["exclude_extensions"] = []string{".md", ".CSV"}
	if _, err := Load(writeConfig(t, values)); err == nil {
		t.Fatal("exclude_extensions removing every extension should be rejected")
	}
}

func TestTaskModelPrecedence(t *testing.T) {
	c := Config{
		OllamaModel:         "normal",
		OllamaModelFast:     "fast",
		OllamaModelExternal: "external",
		OllamaEmbedModel:    "embedder",
		OllamaTaskModels: map[string][]string{
			"expand": {"expand-default", "expand-alt"},
		},
	}
	// Lane defaults.
	for task, want := range map[string]string{
		"enrich": "fast", "analyze": "normal", "embed": "embedder",
		"expand": "expand-default", "freshness": "external",
	} {
		got, err := c.TaskModel(task, false, "")
		if err != nil || got != want {
			t.Fatalf("TaskModel(%s)=%q err=%v, want %q", task, got, err, want)
		}
	}
	// Task default beats lane fallback; without it expand rides external.
	got, err := c.TaskModel("expand", false, "")
	if err != nil || got != "expand-default" {
		t.Fatalf("expand default=%q err=%v", got, err)
	}
	// Explicit external wins over the task default on lane-capable tasks.
	if got, err = c.TaskModel("expand", true, ""); err != nil || got != "external" {
		t.Fatalf("expand external=%q err=%v", got, err)
	}
	// A per-run override wins over everything.
	if got, err = c.TaskModel("analyze", true, "fast"); err != nil || got != "fast" {
		t.Fatalf("override=%q err=%v", got, err)
	}
	// Overrides must name a configured model.
	if _, err = c.TaskModel("analyze", false, "ghost-model"); err == nil {
		t.Fatal("unconfigured override must be rejected")
	}
	// External on a non-lane task falls through to its lane.
	if got, err = c.TaskModel("embed", true, ""); err != nil || got != "embedder" {
		t.Fatalf("embed external=%q err=%v", got, err)
	}
	// An unconfigured lane reports ErrNoTaskModel so batch runs can skip it.
	c.OllamaEmbedModel = ""
	if _, err = c.TaskModel("embed", false, ""); !errors.Is(err, ErrNoTaskModel) {
		t.Fatalf("embed without a model should report ErrNoTaskModel, got %v", err)
	}
}

func TestTaskModelOptionsAndAllModels(t *testing.T) {
	c := Config{
		OllamaModel:         "normal",
		OllamaModelFast:     "fast",
		OllamaModelExternal: "external",
		OllamaTaskModels: map[string][]string{
			"analyze": {"alt1", "fast", "alt2"},
		},
	}
	opts := c.TaskModelOptions("analyze", false)
	// The configured task default leads; the lane default stays selectable.
	want := []string{"alt1", "fast", "alt2", "normal"}
	if len(opts) != len(want) {
		t.Fatalf("options=%v want %v", opts, want)
	}
	for i := range want {
		if opts[i] != want[i] {
			t.Fatalf("options=%v want %v", opts, want)
		}
	}
	for _, m := range []string{"normal", "fast", "external", "alt1", "alt2"} {
		if !c.ValidModel(m) {
			t.Fatalf("%s should be a valid override", m)
		}
	}
	if c.ValidModel("nope") {
		t.Fatal("unknown model must not validate")
	}
}

func TestTaskModelsValidation(t *testing.T) {
	values := requiredConfig()
	values["ollama_task_models"] = map[string]any{"bogus-task": []string{"x"}}
	if _, err := Load(writeConfig(t, values)); err == nil {
		t.Fatal("unknown task key must be rejected")
	}
	values = requiredConfig()
	values["ollama_task_models"] = map[string]any{"expand": []string{}}
	if _, err := Load(writeConfig(t, values)); err == nil {
		t.Fatal("empty alternative list must be rejected")
	}
	values = requiredConfig()
	values["ollama_task_models"] = map[string]any{"expand": []string{""}}
	if _, err := Load(writeConfig(t, values)); err == nil {
		t.Fatal("blank model name must be rejected")
	}
	values = requiredConfig()
	values["ollama_task_models"] = map[string]any{"expand": []string{"m1", "m2"}}
	if _, err := Load(writeConfig(t, values)); err != nil {
		t.Fatalf("valid task model list rejected: %v", err)
	}
}
