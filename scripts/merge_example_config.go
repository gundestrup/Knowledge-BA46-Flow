// Command merge-example-config builds a per-run KBM-flow config for an
// example-knowledge test shelf. It copies a base config and replaces the
// scanner exclusion fields with the values declared in the shelf's
// expected.json, so live `make example-ollama` runs scan the same document
// set as the fake-model corpus tests. Path fields are resolved against the
// base config's directory so the output works from any location.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	base := flag.String("base", "", "base config.json path")
	expected := flag.String("expected", "", "tier expected.json path")
	out := flag.String("out", "", "output config path")
	flag.Parse()
	if *base == "" || *expected == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: merge-example-config -base config.json -expected expected.json -out config.json")
		os.Exit(2)
	}
	if err := run(*base, *expected, *out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(basePath, expectedPath, outPath string) error {
	base, err := readJSON(basePath)
	if err != nil {
		return err
	}
	expected, err := readJSON(expectedPath)
	if err != nil {
		return err
	}
	for _, key := range []string{"exclude", "exclude_extensions", "exclude_directories"} {
		if value, ok := expected[key]; ok {
			base[key] = value
		}
	}
	baseDir, err := filepath.Abs(filepath.Dir(basePath))
	if err != nil {
		return err
	}
	for _, key := range []string{"repository_path", "database_path", "prompt_directory"} {
		absolutize(base, key, baseDir)
	}
	if report, ok := base["report"].(map[string]any); ok {
		for _, key := range []string{"registry_path", "policy_path", "comparison_path"} {
			absolutize(report, key, baseDir)
		}
	}
	data, err := json.MarshalIndent(base, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(outPath, append(data, '\n'), 0600)
}

func readJSON(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return doc, nil
}

func absolutize(doc map[string]any, key, baseDir string) {
	value, ok := doc[key].(string)
	if !ok || value == "" || filepath.IsAbs(value) {
		return
	}
	doc[key] = filepath.Join(baseDir, filepath.FromSlash(value))
}
