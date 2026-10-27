package scanner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/links"
	"knowledge-ba46-flow/internal/paths"
)

// The reverse index is versioned by source content. It is a discovery aid,
// never permission to mutate a referrer whose live hash has changed.
func (w *repoWalker) checkInternalLinks() error {
	docs, err := w.db.Documents(w.ctx)
	if err != nil {
		return err
	}
	known := map[string]database.Document{}
	ids, counts := map[string]string{}, map[string]int{}
	for _, doc := range docs {
		known[doc.Path] = doc
		id := database.DocumentIdentity(doc.FrontmatterJSON)
		ids[doc.Path] = id
		if id != "" {
			counts[id]++
		}
	}
	for rel := range w.paths {
		if err := w.indexDocumentLinks(rel, known[rel].SHA256, ids, counts); err != nil {
			return err
		}
	}
	return nil
}

// indexDocumentLinks rebuilds one document's reference rows and link_issues.
func (w *repoWalker) indexDocumentLinks(rel, wantHash string, ids map[string]string, counts map[string]int) error {
	raw, err := ReadDocument(w.c, rel)
	if err != nil {
		return nil
	}
	fields, body, err := frontmatter.Parse(raw)
	if err != nil {
		return nil // already reported by frontmatter curation
	}
	sum := sha256.Sum256([]byte(frontmatter.Canonical(raw)))
	hash := hex.EncodeToString(sum[:])
	if hash != wantHash {
		return w.db.SetLinkIssues(w.ctx, rel, `["source changed during scan; scan again"]`)
	}
	previous, err := w.db.SourceReferences(w.ctx, rel)
	if err != nil {
		return err
	}
	old := map[string]database.Reference{}
	for _, ref := range previous {
		if ref.SourceSHA256 == hash {
			old[ref.Kind+"\x00"+ref.WrittenTarget] = ref
		}
	}
	issues, refs := w.collectReferences(rel, fields, body, ids, counts, old)
	if id := ids[rel]; id != "" && counts[id] > 1 {
		issues = append(issues, "duplicate document_id; identity-based retargeting is disabled")
	}
	if err := w.db.ReplaceReferences(w.ctx, rel, hash, refs); err != nil {
		return err
	}
	encoded, _ := json.Marshal(issues)
	return w.db.SetLinkIssues(w.ctx, rel, string(encoded))
}

// collectReferences parses graph and body references for one document. A
// prior same-version edge keeps its recorded target identity when resolution
// becomes ambiguous or silently changes meaning after a move/replacement.
func (w *repoWalker) collectReferences(rel string, fields map[string]any, body string, ids map[string]string, counts map[string]int, old map[string]database.Reference) ([]string, []database.Reference) {
	issues, refs := []string{}, []database.Reference{}
	record := func(kind, written, target string, resolved bool) {
		ref := database.Reference{WrittenTarget: written, TargetPath: target, Kind: kind}
		if resolved && counts[ids[target]] == 1 {
			ref.TargetID = ids[target]
		}
		prior, hasPrior := old[kind+"\x00"+written]
		if hasPrior {
			mismatch := resolved && ((prior.TargetID != "" && prior.TargetID != ref.TargetID) ||
				(prior.TargetID == "" && prior.TargetPath != target))
			switch {
			case mismatch:
				issues = append(issues, fmt.Sprintf("link identity changed: %s; review outgoing links after a move/replacement", written))
				ref.TargetID, ref.TargetPath = prior.TargetID, prior.TargetPath
			case !resolved && prior.TargetID != "":
				ref.TargetID, ref.TargetPath = prior.TargetID, prior.TargetPath
			}
		}
		refs = append(refs, ref)
	}
	for _, kind := range []string{"related", "contradicted_by"} {
		for _, target := range frontmatter.StringList(fields, kind) {
			if _, err := paths.CleanRelative(target); err == nil {
				record(kind, target, target, w.paths[target])
			}
		}
	}
	for _, target := range links.InternalTargets(body) {
		resolved, selected := w.resolveBodyTarget(rel, target)
		if !resolved {
			issues = append(issues, "unresolved link target: "+target)
		}
		if selected != "" {
			record("body", target, selected, resolved)
		}
	}
	return issues, refs
}

// resolveBodyTarget finds the repository path a body link resolves to:
// scanned documents for Markdown targets, regular files for assets.
func (w *repoWalker) resolveBodyTarget(rel, target string) (bool, string) {
	selected := ""
	for _, candidate := range links.InternalCandidates(rel, target) {
		if selected == "" {
			selected = candidate
		}
		exists := w.paths[candidate]
		if !links.IsDocumentTarget(target) {
			if local, err := paths.CleanRelative(candidate); err == nil {
				info, err := w.root.Stat(local)
				exists = err == nil && info.Mode().IsRegular()
			}
		}
		if exists {
			return true, candidate
		}
	}
	return false, selected
}
