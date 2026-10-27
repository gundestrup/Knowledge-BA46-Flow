// Package prompts assembles the LLM prompt from editable markdown files on disk.
package prompts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Compose builds the full-analysis prompt: base.md (role + safety rules),
// <mcpServer>.md (the target MCP server's markdown/frontmatter conventions)
// and analyze.md (task + JSON output contract) from dir, then appends the repo
// context and the document under review. Keeping prompts as files lets users
// tune LLM behaviour without recompiling.
func Compose(dir, mcpServer, repoContext, document string) (string, error) {
	return ComposeTask(dir, mcpServer, "analyze.md", repoContext, document)
}

// ComposeTask is Compose with a caller-chosen task file — the fast enrichment
// pass uses enrich.md, the full analysis pass uses analyze.md.
func ComposeTask(dir, mcpServer, taskFile, repoContext, document string) (string, error) {
	parts := make([]string, 0, 3)
	for _, name := range []string{"base.md", mcpServer + ".md", taskFile} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", fmt.Errorf("load prompt %s: %w", name, err)
		}
		parts = append(parts, string(b))
	}
	return strings.Join(parts, "\n\n") + "\n\n# Repository context\n" + repoContext + "\n\n# Document\n" + document, nil
}

// Fingerprint hashes the static prompt files used by ComposeTask plus
// task-specific policy inputs. It deliberately excludes per-document context,
// which is handled by the analyzer's semantic-evidence dependency tracking.
func Fingerprint(dir, mcpServer, taskFile string, policyInputs ...string) (string, error) {
	h := sha256.New()
	for _, name := range []string{"base.md", mcpServer + ".md", taskFile} {
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", fmt.Errorf("load prompt %s: %w", name, err)
		}
		_, _ = h.Write([]byte(name))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(content)
		_, _ = h.Write([]byte{0})
	}
	for _, input := range policyInputs {
		_, _ = h.Write([]byte(input))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
