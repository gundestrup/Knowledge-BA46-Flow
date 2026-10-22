package analyzer

import (
	"crypto/sha256"
	"fmt"

	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
)

func embeddingText(doc database.Document, content string) (string, error) {
	content = frontmatter.Canonical(content)
	if fmt.Sprintf("%x", sha256.Sum256([]byte(content))) != doc.SHA256 {
		return "", fmt.Errorf("document %s changed since scan; scan and retry", doc.Path)
	}
	return content, nil
}
