package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMergeAppliesGoldenExclusionsAndAbsolutizesPaths(t *testing.T) {
	dir := t.TempDir()
	basePath := filepath.Join(dir, "base.json")
	base := `{
  "repository_path": "../example-knowledge",
  "database_path": "../data/example.db",
  "prompt_directory": "../prompts",
  "exclude": ["basic", "intermediate", "e2e"],
  "exclude_directories": ["vendor"],
  "report": {"registry_path": "../repos.yml"}
}`
	if err := os.WriteFile(basePath, []byte(base), 0644); err != nil {
		t.Fatal(err)
	}
	expectedPath := filepath.Join(dir, "expected.json")
	expected := `{
  "exclude": ["sandbox", "drafts/**"],
  "exclude_extensions": [".markdown"]
}`
	if err := os.WriteFile(expectedPath, []byte(expected), 0644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(dir, "out.json")
	if err := run(basePath, expectedPath, outPath); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	exclude, _ := got["exclude"].([]any)
	if len(exclude) != 2 || exclude[0] != "sandbox" || exclude[1] != "drafts/**" {
		t.Fatalf("exclude=%v", got["exclude"])
	}
	exts, _ := got["exclude_extensions"].([]any)
	if len(exts) != 1 || exts[0] != ".markdown" {
		t.Fatalf("exclude_extensions=%v", got["exclude_extensions"])
	}
	if dirs, _ := got["exclude_directories"].([]any); len(dirs) != 1 || dirs[0] != "vendor" {
		t.Fatalf("exclude_directories should keep base value, got %v", got["exclude_directories"])
	}
	for _, key := range []string{"repository_path", "database_path", "prompt_directory"} {
		if value, _ := got[key].(string); !filepath.IsAbs(value) {
			t.Errorf("%s=%q is not absolute", key, value)
		}
	}
	report, _ := got["report"].(map[string]any)
	if value, _ := report["registry_path"].(string); !filepath.IsAbs(value) {
		t.Errorf("report.registry_path=%q is not absolute", value)
	}
}

func TestMergeErrorPaths(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.json")
	if err := os.WriteFile(valid, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	malformed := filepath.Join(dir, "malformed.json")
	if err := os.WriteFile(malformed, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.json")

	if err := run(filepath.Join(dir, "absent.json"), valid, out); err == nil {
		t.Error("missing base should fail")
	}
	if err := run(malformed, valid, out); err == nil {
		t.Error("malformed base should fail")
	}
	if err := run(valid, malformed, out); err == nil {
		t.Error("malformed expected should fail")
	}
	// Output in a directory that does not exist fails the write.
	if err := run(valid, valid, filepath.Join(dir, "no-dir", "out.json")); err == nil {
		t.Error("unwritable output path should fail")
	}
}
