package prompts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testMCP = "knowledge-base-mcp-server"

func writeAll(t *testing.T, dir string) {
	t.Helper()
	os.WriteFile(filepath.Join(dir, "base.md"), []byte("BASE"), 0644)
	os.WriteFile(filepath.Join(dir, testMCP+".md"), []byte("MCP"), 0644)
	os.WriteFile(filepath.Join(dir, "analyze.md"), []byte("TASK"), 0644)
}

func TestCompose(t *testing.T) {
	dir := t.TempDir()
	writeAll(t, dir)
	got, err := Compose(dir, testMCP, "CONTEXT", "DOCUMENT")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"BASE", "MCP", "TASK", "CONTEXT", "DOCUMENT"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q", want)
		}
	}
	// Order: base -> server conventions -> task -> context -> document.
	if strings.Index(got, "BASE") >= strings.Index(got, "MCP") ||
		strings.Index(got, "MCP") >= strings.Index(got, "TASK") ||
		strings.Index(got, "TASK") >= strings.Index(got, "CONTEXT") {
		t.Fatalf("unexpected section order:\n%s", got)
	}
}

func TestComposeRequiresBasePrompt(t *testing.T) {
	dir := t.TempDir()
	if _, err := Compose(dir, testMCP, "CONTEXT", "DOCUMENT"); err == nil ||
		!strings.Contains(err.Error(), "base") {
		t.Fatalf("expected a base prompt error, got %v", err)
	}
}

func TestComposeRequiresMCPServerPrompt(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "base.md"), []byte("BASE"), 0644)
	if _, err := Compose(dir, testMCP, "CONTEXT", "DOCUMENT"); err == nil ||
		!strings.Contains(err.Error(), testMCP) {
		t.Fatalf("expected an MCP prompt error, got %v", err)
	}
}

func TestComposeRequiresAnalyzePrompt(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "base.md"), []byte("BASE"), 0644)
	os.WriteFile(filepath.Join(dir, testMCP+".md"), []byte("MCP"), 0644)
	if _, err := Compose(dir, testMCP, "CONTEXT", "DOCUMENT"); err == nil ||
		!strings.Contains(err.Error(), "analyze") {
		t.Fatalf("expected an analyze prompt error, got %v", err)
	}
}

func TestFingerprintTracksOnlyStaticTaskPromptInputs(t *testing.T) {
	dir := t.TempDir()
	writeAll(t, dir)
	first, err := Fingerprint(dir, testMCP, "analyze.md", "write_scope=metadata-links")
	if err != nil {
		t.Fatal(err)
	}
	changedScope, err := Fingerprint(dir, testMCP, "analyze.md", "write_scope=content")
	if err != nil || first == changedScope {
		t.Fatalf("policy input did not affect fingerprint: %q %q err=%v", first, changedScope, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "analyze.md"), []byte("changed task"), 0644); err != nil {
		t.Fatal(err)
	}
	changedPrompt, err := Fingerprint(dir, testMCP, "analyze.md", "write_scope=metadata-links")
	if err != nil || first == changedPrompt {
		t.Fatalf("task prompt edit did not affect fingerprint: %q %q err=%v", first, changedPrompt, err)
	}
}
