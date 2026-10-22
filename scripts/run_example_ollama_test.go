package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestExampleRunnerRejectsUnsafeOllamaURLsBeforeOllama(t *testing.T) {
	binDir := t.TempDir()
	ollamaPath := filepath.Join(binDir, "ollama")
	ollamaStub := "#!/bin/sh\nprintf called > \"$OLLAMA_STUB_MARKER\"\nexit 0\n"
	if err := os.WriteFile(ollamaPath, []byte(ollamaStub), 0700); err != nil {
		t.Fatal(err)
	}
	for _, rawURL := range []string{
		"http://localhost:11434@remote.invalid:11434",
		"http://127.0.0.1:11434/path",
		"http://localhost:0",
	} {
		t.Run(rawURL, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "ollama-called")
			var env []string
			for _, entry := range os.Environ() {
				if strings.HasPrefix(entry, "PATH=") ||
					strings.HasPrefix(entry, "OLLAMA_BASE_URL=") ||
					strings.HasPrefix(entry, "OLLAMA_STUB_MARKER=") {
					continue
				}
				env = append(env, entry)
			}
			env = append(env,
				"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"OLLAMA_STUB_MARKER="+marker,
				"OLLAMA_BASE_URL="+rawURL,
			)
			cmd := exec.Command("./run-example-ollama.sh", "basic")
			cmd.Env = env
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("runner accepted unsafe URL %q; output=%s", rawURL, output)
			}
			if !strings.Contains(string(output), "loopback") && !strings.Contains(string(output), "port is out of range") {
				t.Fatalf("runner did not explain URL rejection: %s", output)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("ollama command was reached before rejecting URL %q: stat error=%v", rawURL, err)
			}
		})
	}
}
