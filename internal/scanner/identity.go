package scanner

import (
	"context"
	"encoding/json"
	"os"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/mcppolicy"
	"knowledge-ba46-flow/internal/paths"
)

// ensureDocumentID assigns a UUID to a document that lacks one — the second
// tool-owned repository write, alongside the kbm block. The document_id line
// is excluded from the canonical content hash, so insertion is invisible to
// change detection and never invalidates pending suggestions. Skips (with a
// stored issue) when the frontmatter does not parse, the existing ID is
// malformed (never overwrite human data), the repository write policy denies
// mutations, or the atomic write fails.
func (w *repoWalker) ensureDocumentID(rel, local, raw string) (string, string) {
	fields, _, err := frontmatter.Parse(raw)
	if err != nil {
		return raw, "" // the parse issue is already recorded by inspectContent
	}
	id, err := frontmatter.DocumentID(fields)
	if err != nil {
		return raw, "invalid document_id: " + err.Error()
	}
	if id != "" {
		return raw, ""
	}
	if err := mcppolicy.AssertMutationAllowed(w.root, rel); err != nil {
		return raw, "document_id assignment denied by .kb-policy.json"
	}
	updated, err := frontmatter.AddDocumentID(raw, frontmatter.NewDocumentID())
	if err != nil {
		return raw, "document_id assignment failed: " + err.Error()
	}
	if err := paths.AtomicWrite(w.root, local, []byte(updated)); err != nil {
		return raw, "document_id assignment failed: " + err.Error()
	}
	w.result.IDsAssigned++
	return updated, ""
}

// appendIssue adds a scanner-level notice to the stored frontmatter issue JSON.
func appendIssue(issuesJSON, issue string) string {
	var issues []string
	_ = json.Unmarshal([]byte(issuesJSON), &issues)
	issues = append(issues, issue)
	out, _ := json.Marshal(issues)
	return string(out)
}

// ObservedRetarget verifies recorded identity against the live target. A
// renamed path replaced by another UUID must never inherit the old links.
func ObservedRetarget(ctx context.Context, c config.Config, db *database.DB, old string) (string, bool, error) {
	local, err := paths.CleanRelative(old)
	if err != nil {
		return "", false, nil // an escaping candidate cannot be rename evidence
	}
	root, err := os.OpenRoot(c.RepositoryPath)
	if err != nil {
		return "", false, err
	}
	defer root.Close()
	if _, err := root.Lstat(local); err == nil {
		return "", false, nil
	} else if !os.IsNotExist(err) {
		return "", false, err
	}
	target, expectedID, ok := db.RetargetEvidence(ctx, old)
	if !ok {
		return "", false, nil
	}
	if _, err := paths.CleanRelative(target); err != nil {
		return "", false, nil
	}
	raw, err := ReadDocument(c, target)
	if err != nil {
		return "", false, err
	}
	if expectedID != "" {
		fields, _, err := frontmatter.Parse(raw)
		if err != nil {
			return "", false, err
		}
		actual, err := frontmatter.DocumentID(fields)
		if err != nil || actual != expectedID {
			return "", false, nil
		}
	}
	return target, true, nil
}
