package main

// Drift checks: the CLI flag set, the /api/ops op list and the config JSON
// schema each live in exactly one place, but they are documented in three.
// These tests walk the real command tree, the real op list and the real
// struct tags and compare them against the shipped docs — both directions —
// so an option added to the UI or CLI without docs (or docs describing an
// option that no longer exists) fails `make check`.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/server"
)

// docsCorpus concatenates the user-facing docs drift is checked against.
func docsCorpus(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, name := range []string{"README.md"} {
		data, err := os.ReadFile(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		b.Write(data)
	}
	entries, err := os.ReadDir(filepath.Join("..", "..", "docs"))
	if err != nil {
		t.Fatalf("read docs dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join("..", "..", "docs", e.Name()))
		if err != nil {
			t.Fatalf("read docs/%s: %v", e.Name(), err)
		}
		b.Write(data)
	}
	return b.String()
}

// apiToCLI maps every /api/ops op to the CLI command that performs the same
// job. Names differ where the API kept a different spelling; extend the map
// when an op is added, never by weakening the test.
var apiToCLI = map[string]string{
	"scan":       "scan",
	"analyze":    "analyze",
	"run":        "run",
	"link-check": "check-links",
	"enrich":     "enrich",
	"embed":      "embed",
	"expand":     "expand",
	"freshness":  "freshness",
	"all":        "tasks",
}

// cliJobCommands are the CLI commands that claim a row in the jobs table;
// each must stay reachable over HTTP. `report` uses /api/report/run rather
// than /api/ops — asserted separately.
var cliJobCommands = map[string]string{
	"scan":        "scan",
	"check-links": "link-check",
	"analyze":     "analyze",
	"run":         "run",
	"enrich":      "enrich",
	"embed":       "embed",
	"expand":      "expand",
	"freshness":   "freshness",
	"tasks":       "all",
	"report":      "report",
}

func findCommand(root *cobra.Command, name string) *cobra.Command {
	for _, c := range root.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

func TestAPIOpsHaveCLICommands(t *testing.T) {
	root := rootCmd()
	for _, op := range server.ValidOps() {
		cli, ok := apiToCLI[op]
		if !ok {
			t.Errorf("API op %q has no CLI mapping in apiToCLI — wire one or extend the map", op)
			continue
		}
		if findCommand(root, cli) == nil {
			t.Errorf("API op %q maps to missing CLI command %q", op, cli)
		}
	}
	if len(apiToCLI) != len(server.ValidOps()) {
		t.Errorf("apiToCLI has %d entries, API accepts %d ops — stale mapping?", len(apiToCLI), len(server.ValidOps()))
	}
}

func TestCLIJobCommandsReachableViaAPI(t *testing.T) {
	root := rootCmd()
	valid := map[string]bool{}
	for _, op := range server.ValidOps() {
		valid[op] = true
	}
	valid["report"] = true // separate /api/report/run route, not an /api/ops op
	for cli, op := range cliJobCommands {
		if findCommand(root, cli) == nil {
			t.Errorf("cliJobCommands names %q but no such CLI command exists", cli)
			continue
		}
		if !valid[op] {
			t.Errorf("CLI command %q maps to API op %q the server does not accept", cli, op)
		}
	}
}

// builtinFlags are added by cobra itself, not part of the feature surface.
var builtinFlags = map[string]bool{"help": true, "version": true}

func TestCLIFlagsDocumented(t *testing.T) {
	corpus := docsCorpus(t)
	root := rootCmd()
	var missing []string
	check := func(cmd *cobra.Command) {
		visit := func(f *pflag.Flag) {
			if builtinFlags[f.Name] {
				return
			}
			if !strings.Contains(corpus, "--"+f.Name) {
				missing = append(missing, cmd.Name()+" --"+f.Name)
			}
		}
		cmd.Flags().VisitAll(visit)
		cmd.PersistentFlags().VisitAll(visit)
	}
	check(root) // persistent --config lives on the root
	for _, c := range root.Commands() {
		check(c)
	}
	if len(missing) > 0 {
		t.Errorf("CLI flags absent from README.md/docs: %s", strings.Join(missing, ", "))
	}
}

var flagToken = regexp.MustCompile(`--[a-z][a-z0-9-]*`)

func TestDocumentedFlagsExist(t *testing.T) {
	root := rootCmd()
	known := map[string]bool{}
	collect := func(cmd *cobra.Command) {
		cmd.Flags().VisitAll(func(f *pflag.Flag) { known[f.Name] = true })
		cmd.PersistentFlags().VisitAll(func(f *pflag.Flag) { known[f.Name] = true })
	}
	collect(root)
	for _, c := range root.Commands() {
		collect(c)
	}
	for name := range builtinFlags {
		known[name] = true
	}
	// Only lines invoking KBM-flow count — docs also show ollama, npm, docker
	// and go flags that are none of our business.
	var unknown []string
	for _, line := range strings.Split(docsCorpus(t), "\n") {
		if !strings.Contains(line, "KBM-flow") {
			continue
		}
		for _, tok := range flagToken.FindAllString(line, -1) {
			name := strings.TrimPrefix(tok, "--")
			if !known[name] {
				unknown = append(unknown, tok+" (line: "+strings.TrimSpace(line)+")")
			}
		}
	}
	if len(unknown) > 0 {
		t.Errorf("docs describe flags no KBM-flow command accepts: %s", strings.Join(unknown, "; "))
	}
}

func TestCLICommandsDocumented(t *testing.T) {
	corpus := docsCorpus(t)
	// completion is a cobra built-in; everything else is shipped feature surface.
	builtin := map[string]bool{"completion": true, "help": true}
	for _, c := range rootCmd().Commands() {
		if builtin[c.Name()] {
			continue
		}
		if !strings.Contains(corpus, "`"+c.Name()+"`") {
			t.Errorf("CLI command %q is never mentioned (backticked) in README.md/docs", c.Name())
		}
	}
}

// configJSONKeys returns every config-file key: top-level tags plus dotted
// keys for nested objects (report.registry_path, validate.exempt).
func configJSONKeys() (top, nested map[string]bool) {
	top, nested = map[string]bool{}, map[string]bool{}
	var walk func(reflect.Type, string)
	walk = func(typ reflect.Type, prefix string) {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				continue
			}
			if prefix == "" {
				top[name] = true
			} else {
				nested[prefix+name] = true
			}
			if f.Type.Kind() == reflect.Struct {
				walk(f.Type, name+".")
			}
		}
	}
	walk(reflect.TypeOf(config.Config{}), "")
	return top, nested
}

func TestConfigFieldsDocumented(t *testing.T) {
	corpus := docsCorpus(t)
	top, nested := configJSONKeys()
	var missing []string
	for key := range top {
		if !strings.Contains(corpus, "`"+key+"`") {
			missing = append(missing, key)
		}
	}
	for key := range nested {
		if !strings.Contains(corpus, "`"+key+"`") {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("config fields absent from README.md/docs: %s", strings.Join(missing, ", "))
	}
}

// docFieldCell matches a leading table cell like `repository_path` or
// `report.registry_path` — the field tables in docs/CONFIGURATION.md.
var docFieldCell = regexp.MustCompile("^[ \t]*\\|[ \t]*`([a-z][a-z_]*(?:\\.[a-z_]+)?)`")

// TestExampleConfigCoversAllFields keeps example-configs/config.example-data.json
// the complete reference: every config.json key — including nested report.* and
// validate.* — must appear there so new fields are never silently undocumented.
func TestExampleConfigCoversAllFields(t *testing.T) {
	examplePath := filepath.Join("..", "..", "example-configs", "config.example-data.json")
	data, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatalf("read config.example-data.json: %v", err)
	}
	var example map[string]any
	if err := json.Unmarshal(data, &example); err != nil {
		t.Fatalf("parse config.example-data.json: %v", err)
	}
	// The complete reference must also be a *valid* config — full Load
	// catches bad enum values, malformed patterns and invalid task names
	// that a key-presence check cannot.
	if _, err := config.Load(examplePath); err != nil {
		t.Fatalf("config.example-data.json failed to load: %v", err)
	}
	top, nested := configJSONKeys()
	var missing []string
	for key := range top {
		if _, ok := example[key]; !ok {
			missing = append(missing, key)
		}
	}
	for key := range nested {
		parent, leaf, _ := strings.Cut(key, ".")
		sub, ok := example[parent].(map[string]any)
		if !ok {
			missing = append(missing, key)
			continue
		}
		if _, ok := sub[leaf]; !ok {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("config.example-data.json missing fields: %s", strings.Join(missing, ", "))
	}
}

// TestConfigFieldsHaveBehavioralTests requires every config field to be
// exercised by a test: its JSON key or its Go field name must appear in at
// least one *_test.go other than this file (which would match its own
// field list). Presence proves only that a test touches the option —
// writing the meaningful assertion stays the reviewer's job, but a field
// with zero test references fails loudly here.
func TestConfigFieldsHaveBehavioralTests(t *testing.T) {
	root := filepath.Join("..", "..")
	var corpus strings.Builder
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, "drift_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		corpus.Write(data)
		return nil
	})
	if err != nil {
		t.Fatalf("walk test files: %v", err)
	}
	tests := corpus.String()

	var missing []string
	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			key, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if key == "" || key == "-" {
				continue
			}
			if !strings.Contains(tests, key) && !strings.Contains(tests, f.Name) {
				missing = append(missing, key)
			}
			if f.Type.Kind() == reflect.Struct {
				walk(f.Type)
			}
		}
	}
	walk(reflect.TypeOf(config.Config{}))
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("config fields with no test coverage: %s", strings.Join(missing, ", "))
	}
}

func TestDocumentedConfigFieldsExist(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "CONFIGURATION.md"))
	if err != nil {
		t.Fatalf("read CONFIGURATION.md: %v", err)
	}
	top, nested := configJSONKeys()
	var unknown []string
	for _, line := range strings.Split(string(data), "\n") {
		m := docFieldCell.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if strings.Contains(m[1], ".") {
			if !nested[m[1]] {
				unknown = append(unknown, m[1])
			}
			continue
		}
		if !top[m[1]] && !nested[m[1]] {
			unknown = append(unknown, m[1])
		}
	}
	if len(unknown) > 0 {
		t.Errorf("CONFIGURATION.md documents fields config.Config does not have: %s", strings.Join(unknown, ", "))
	}
}
