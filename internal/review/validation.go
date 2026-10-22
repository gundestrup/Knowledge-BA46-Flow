package review

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/proposal"
	"knowledge-ba46-flow/internal/taxonomy"
)

// Revalidate even old accepted rows against the current source and link
// targets. No directories or temporary files are created before this passes.
func checkProposedContent(root *os.Root, path, proposed string, c config.Config) error {
	info, err := root.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("source is not a regular file: %s", path)
	}
	maxBytes := c.MaxDocumentBytes
	if maxBytes <= 0 {
		maxBytes = 200000
	}
	f, err := root.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	content, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return err
	}
	if int64(len(content)) > maxBytes {
		return fmt.Errorf("document exceeds max_document_bytes (%d)", maxBytes)
	}
	if err := proposal.ValidateRewrite(string(content), proposed); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	vocabulary, err := taxonomy.LoadFromRoot(root)
	if err != nil {
		return fmt.Errorf("load tag taxonomy: %w", err)
	}
	if err := vocabulary.ValidateTagChanges(string(content), proposed); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return proposal.ValidateLinks(proposed, func(target string) error {
		extensions := c.Extensions
		if len(extensions) == 0 {
			extensions = []string{".md", ".markdown"}
		}
		document := false
		for _, ext := range extensions {
			if strings.EqualFold(filepath.Ext(target), ext) {
				document = true
				break
			}
		}
		if !document {
			return fmt.Errorf("not a configured document extension")
		}
		info, err := root.Stat(target)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("not a regular repository document")
		}
		return nil
	})
}
