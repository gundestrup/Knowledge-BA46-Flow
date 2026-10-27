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
	if doc.SemanticSHA256 != "" {
		currentSemantic := frontmatter.SemanticHash(content)
		if currentSemantic != doc.SemanticSHA256 {
			return "", fmt.Errorf("document %s semantic input changed since scan (expected %s, current %s); scan and retry",
				doc.Path, doc.SemanticSHA256, currentSemantic)
		}
	}
	return frontmatter.SemanticContent(content), nil
}
