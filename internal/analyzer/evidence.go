// The evidence machinery: what an analysis prompt actually consumed
// (recording) and which unchanged documents must be re-analyzed when that
// evidence drifts (the dependents sweep). Recorded v2 sets are exact; rows
// without a recording keep the conservative heuristic sweep.
package analyzer

import (
	"cmp"
	"encoding/json"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/scanner"
	"knowledge-ba46-flow/internal/similarity"
)

// analysisEvidence records which documents an analysis prompt actually
// consumed. Fm/Full distinguish candidate annotations and body-derived
// evidence; inbound and sibling paths are stored separately because they are
// path-only context and should invalidate on set changes, not source-content
// changes. ValidationContext captures the rendered local link/metadata issues.
type analysisEvidence struct {
	Version           int      `json:"v,omitempty"`
	Fm                []string `json:"fm"`
	Full              []string `json:"full"`
	Inbound           []string `json:"inbound,omitempty"`
	Siblings          []string `json:"siblings,omitempty"`
	ValidationContext string   `json:"validation_context,omitempty"`
}

func fmLinkTargets(fields map[string]any, keys ...string) []string {
	var out []string
	for _, key := range keys {
		out = append(out, frontmatter.StringList(fields, key)...)
	}
	return out
}

func marshalAnalysisEvidence(ranked []candidate, inbound, siblings []string, mergeBodies map[string]string, entityPaths []string, validationContext string) string {
	fm := make(map[string]bool)
	full := make(map[string]bool)
	for _, c := range ranked {
		fm[c.doc.Path] = true
		// The candidate annotation includes shingle/embedding similarity;
		// these scores consume body semantics even when no target body is
		// included verbatim.
		if c.sim > 0 || c.rel > 0 {
			full[c.doc.Path] = true
		}
	}
	for path := range mergeBodies {
		full[path] = true
	}
	for _, path := range entityPaths {
		full[path] = true
	}
	delete(fm, "")
	delete(full, "")
	raw, _ := json.Marshal(analysisEvidence{
		Version: 2, Fm: slices.Sorted(maps.Keys(fm)), Full: slices.Sorted(maps.Keys(full)),
		Inbound: sortedUnique(inbound), Siblings: sortedUnique(siblings),
		ValidationContext: validationContext,
	})
	return string(raw)
}

func sortedUnique(values []string) []string {
	out := slices.Clone(values)
	slices.Sort(out)
	return slices.Compact(out)
}

func parseAnalysisEvidenceRecord(raw string) (analysisEvidence, bool) {
	if raw == "" {
		return analysisEvidence{}, false
	}
	var ev analysisEvidence
	if json.Unmarshal([]byte(raw), &ev) != nil {
		return analysisEvidence{}, false
	}
	return ev, true
}

// parseAnalysisEvidence decodes the stored evidence sets. A missing or
// malformed value means the row predates evidence recording — callers fall
// back to the heuristic sweep.
func parseAnalysisEvidence(raw string) (fm, full map[string]bool, ok bool) {
	ev, ok := parseAnalysisEvidenceRecord(raw)
	if !ok {
		return nil, nil, false
	}
	fm = make(map[string]bool, len(ev.Fm))
	for _, p := range ev.Fm {
		fm[p] = true
	}
	full = make(map[string]bool, len(ev.Full))
	for _, p := range ev.Full {
		full[p] = true
	}
	return fm, full, true
}

// evidenceDependents sweeps for documents whose analysis context changed even
// though their own inputs did not. Documents with a recorded evidence set are
// invalidated exactly — a doc is re-analyzed only when a path its prompt
// actually consumed changed. Two seed sets apply: changedPaths covers
// full-semantic drift (body-level evidence); changedFmPaths covers frontmatter
// drift since the last analysis stamp (annotation-level evidence) — a linker
// sees a target's title/tags/summary, not its body. Documents without recorded
// evidence (predating the column) fall back to the conservative heuristic
// sweep until they are re-analyzed and stamped with their real evidence set.
func intersectsAny(set, seeds map[string]bool) bool {
	for path := range set {
		if seeds[path] {
			return true
		}
	}
	return false
}

func intersectsAfter(set map[string]bool, seeds map[string]int64, generation int64) bool {
	for path := range set {
		if seeds[path] > generation {
			return true
		}
	}
	return false
}

func intersectsSliceAfter(paths []string, seeds map[string]int64, generation int64) bool {
	for _, path := range paths {
		if seeds[path] > generation {
			return true
		}
	}
	return false
}

// sweepSeeds carries the derived change sets through the dependents sweep —
// classified once per run from the scan diff and the evidence generations,
// then consumed read-only per document by sweepDocument/isEvidenceDependent.
// Bundling them keeps the dependency arms named and the function signatures
// stable when a new seed kind appears.
type sweepSeeds struct {
	changed             map[string]bool  // semantic/body drift → body-level evidence
	changedFm           map[string]bool  // frontmatter drift → annotation-level evidence
	changedNewFm        map[string]bool  // never-analyzed docs → heuristic-only seeds
	analysisPending     map[string]bool  // already in the direct queue
	directPaths         map[string]bool  // direct queue membership (dependent vs direct)
	inbound             map[string]bool  // linkers to fm-changed targets (legacy arm)
	inboundNew          map[string]bool  // linkers to brand-new targets
	related             map[string]bool  // embedding-projection neighbors
	removed             map[string]int64 // missing docs → removal generation
	removedInbound      map[string]int64 // linkers to removed targets → generation
	acknowledgeRemovals bool             // real runs stamp; estimates only preview
}

// evidenceDependents sweeps for documents whose analysis context changed even
// though their own inputs did not. The changed/new/removed seeds are exact:
// docs with a recorded evidence set invalidate only on real evidence overlap,
// legacy rows keep the conservative heuristic sweep until they are stamped.
func (p *pipeline) evidenceDependents(docs []database.Document, acknowledgeRemovals bool) ([]database.Document, error) {
	changed, changedFm, changedNewFm, analysisPending := p.pendingEvidencePaths(docs)
	seeds := &sweepSeeds{
		changed:             visiblePathSet(changed, p.visiblePaths),
		changedFm:           visiblePathSet(changedFm, p.visiblePaths),
		changedNewFm:        changedNewFm,
		analysisPending:     analysisPending,
		directPaths:         make(map[string]bool, len(docs)),
		acknowledgeRemovals: acknowledgeRemovals,
	}
	for _, doc := range docs {
		seeds.directPaths[doc.Path] = true
	}
	var err error
	seeds.removed, err = p.removedPaths()
	if err != nil {
		return nil, err
	}
	if len(seeds.changed) == 0 && len(seeds.changedFm) == 0 && !p.needsRemovalSweep(p.removalGeneration) {
		return nil, nil
	}
	seeds.inbound, seeds.inboundNew, seeds.removedInbound, err = p.inboundMaps(seeds.changedFm, changedNewFm, seeds.removed)
	if err != nil {
		return nil, err
	}
	seeds.related = p.embeddingDependents(seeds.changed)
	var dependents []database.Document
	for _, doc := range p.allDocs {
		if p.ctx.Err() != nil {
			return nil, p.ctx.Err()
		}
		dependent, err := p.sweepDocument(doc, seeds)
		if err != nil {
			return nil, err
		}
		if dependent {
			dependents = append(dependents, doc)
		}
	}
	return dependents, nil
}

func visiblePathSet(paths, visible map[string]bool) map[string]bool {
	filtered := make(map[string]bool)
	for path := range paths {
		if visible[path] {
			filtered[path] = true
		}
	}
	return filtered
}

func (p *pipeline) needsRemovalSweep(generation int64) bool {
	for _, doc := range p.allDocs {
		if doc.Status != "missing" && p.laneRemovalGeneration(doc) < generation {
			return true
		}
	}
	return false
}

// sweepDocument applies the seeds to one corpus document: it may mark the doc
// as an evidence-dependent for this run (forceAnalysis) and reports whether
// that marking was genuinely new work (not already a direct queue member).
// Independently it acknowledges the lane's removal generation once the doc is
// confirmed stable or unreachable.
func (p *pipeline) sweepDocument(doc database.Document, seeds *sweepSeeds) (bool, error) {
	if doc.Status == "missing" {
		return false, nil
	}
	seenGeneration := p.laneRemovalGeneration(doc)
	if !p.visiblePaths[doc.Path] {
		return false, p.acknowledgeRemoval(doc.Path, seenGeneration, seeds.acknowledgeRemovals)
	}
	if seeds.analysisPending[doc.Path] || p.isLanePending(doc) {
		return false, nil
	}
	dependent, skip, err := p.isEvidenceDependent(&doc, seeds)
	if err != nil {
		return false, err
	}
	if skip {
		if !p.currentlyVisible(doc.Path) {
			return false, p.acknowledgeRemoval(doc.Path, seenGeneration, seeds.acknowledgeRemovals)
		}
		return false, nil
	}
	if !dependent || !p.currentlyVisible(doc.Path) {
		return false, p.acknowledgeRemoval(doc.Path, seenGeneration, seeds.acknowledgeRemovals)
	}
	p.forceAnalysis[doc.Path] = true
	return !seeds.directPaths[doc.Path], nil
}

func (p *pipeline) acknowledgeRemoval(path string, seenGeneration int64, acknowledge bool) error {
	if !acknowledge || seenGeneration >= p.removalGeneration {
		return nil
	}
	return p.db.MarkRemovalChecked(p.ctx, path, p.external, p.removalGeneration)
}

// inboundMaps builds the linker sets for the sweep: sources linking to any
// frontmatter-changed target (legacy sweep), to new-seed targets only, and
// to newly removed targets.
func (p *pipeline) inboundMaps(changedFm, changedNewFm map[string]bool, removed map[string]int64) (inbound, inboundNew map[string]bool, removedInbound map[string]int64, err error) {
	inbound = make(map[string]bool)
	inboundNew = make(map[string]bool)
	removedInbound = make(map[string]int64)
	collect := func(targets map[string]bool, mark func(string)) error {
		for target := range targets {
			sources, err := p.db.InboundLinks(p.ctx, target)
			if err != nil {
				return err
			}
			for _, source := range sources {
				if p.visiblePaths[source] {
					mark(source)
				}
			}
		}
		return nil
	}
	if err := collect(changedFm, func(s string) { inbound[s] = true }); err != nil {
		return nil, nil, nil, err
	}
	if err := collect(changedNewFm, func(s string) { inboundNew[s] = true }); err != nil {
		return nil, nil, nil, err
	}
	for target, eventGeneration := range removed {
		sources, err := p.db.InboundLinks(p.ctx, target)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, source := range sources {
			if p.visiblePaths[source] && eventGeneration > removedInbound[source] {
				removedInbound[source] = eventGeneration
			}
		}
	}
	return inbound, inboundNew, removedInbound, nil
}

// removedPaths returns currently missing paths and their monotonic deletion
// generations. The per-lane acknowledgement makes events idempotent across
// repeated runs and robust to clock adjustments or multiple scans.
func (p *pipeline) removedPaths() (map[string]int64, error) {
	removed := make(map[string]int64)
	docs, err := p.db.MissingDocuments(p.ctx)
	if err != nil {
		return nil, err
	}
	for _, doc := range docs {
		if doc.Generation <= p.removalGeneration {
			removed[doc.Path] = doc.Generation
		}
	}
	return removed, nil
}

// isEvidenceDependent decides whether an unchanged, non-pending doc must be
// re-analyzed because evidence it consumed (or would newly consume) drifted.
// Recorded evidence is exact; rows without a recording keep the conservative
// heuristic sweep. skip means the doc is unreadable or excluded from LLM
// context.
func (p *pipeline) isEvidenceDependent(doc *database.Document, seeds *sweepSeeds) (dep, skip bool, err error) {
	ev, recorded := parseAnalysisEvidenceRecord(p.laneEvidenceJSON(*doc))
	fmEv := stringSet(ev.Fm)
	fullEv := stringSet(ev.Full)
	seenGeneration := p.laneRemovalGeneration(*doc)
	exactEvidence := recorded && ev.Version >= 2
	if exactEvidence && (intersectsAny(fmEv, seeds.changedFm) || intersectsAny(fullEv, seeds.changed) ||
		intersectsAfter(fmEv, seeds.removed, seenGeneration) || intersectsAfter(fullEv, seeds.removed, seenGeneration)) {
		return true, false, nil
	}
	if recorded && !exactEvidence &&
		(intersectsAfter(fmEv, seeds.removed, seenGeneration) || intersectsAfter(fullEv, seeds.removed, seenGeneration)) {
		return true, false, nil
	}
	content, err := scanner.ReadDocument(p.c, doc.Path)
	if err != nil {
		return false, true, nil
	}
	fm, _, err := frontmatter.Parse(content)
	if err != nil || frontmatter.NoLLMContext(fm) {
		return false, true, nil
	}
	if exactEvidence {
		contextChanged, err := p.recordedContextChanged(doc, fm, content, ev)
		if err != nil {
			return false, false, err
		}
		if contextChanged {
			return true, false, nil
		}
		// New seeds and semantic projections model evidence the prompt would
		// newly cite; persisted evidence cannot contain it yet.
		return seeds.inboundNew[doc.Path] ||
			p.dependsOnChangedEvidence(*doc, fm, content, seeds.changed, seeds.changedNewFm, seeds.related), false, nil
	}
	if recorded {
		// Older evidence flattened candidates and inbound links together.
		// Recover clear newly-added inbound paths, but do not interpret every
		// old candidate path as proof that its body was injected.
		inboundAdded, err := p.legacyInboundAdded(doc.Path, fmEv)
		if err != nil {
			return false, false, err
		}
		if inboundAdded {
			return true, false, nil
		}
	}
	return seeds.inbound[doc.Path] || seeds.removedInbound[doc.Path] > seenGeneration ||
		intersectsSliceAfter(fmLinkTargets(fm, "related", "contradicted_by", "links"), seeds.removed, seenGeneration) ||
		p.dependsOnChangedEvidence(*doc, fm, content, seeds.changed, seeds.changedFm, seeds.related), false, nil
}

func (p *pipeline) legacyInboundAdded(path string, oldEvidence map[string]bool) (bool, error) {
	inbound, err := p.llmVisibleInbound(path)
	if err != nil {
		return false, err
	}
	for _, source := range inbound {
		if !oldEvidence[source] {
			return true, nil
		}
	}
	return false, nil
}

func (p *pipeline) recordedContextChanged(doc *database.Document, fm map[string]any, content string, ev analysisEvidence) (bool, error) {
	inbound, err := p.llmVisibleInbound(doc.Path)
	if err != nil {
		return false, err
	}
	if !equalStrings(ev.Inbound, inbound) {
		return true, nil
	}
	siblings, err := scanner.SiblingFiles(p.c, doc.Path)
	if err != nil {
		return false, err
	}
	_, siblingPaths := siblingContext(siblings, content)
	if !equalStrings(ev.Siblings, siblingPaths) {
		return true, nil
	}
	return ev.ValidationContext != frontmatterContext(*doc, fm, p.knownDocs), nil
}

func equalStrings(a, b []string) bool {
	return slices.Equal(sortedUnique(a), sortedUnique(b))
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		if value != "" {
			set[value] = true
		}
	}
	return set
}

// embeddingEvidenceFloor returns the evidenceFloorQuantile cosine of the
// sampled document-pair distribution under the live embedding model — the
// per-run calibration of "unusually close." A zero floor (sparse or tiny
// corpus) degrades gracefully to a pure top-k neighborhood.
func (p *pipeline) embeddingEvidenceFloor() float64 {
	paths := slices.Sorted(maps.Keys(p.embeds))
	n := len(paths)
	if n < 4 {
		return 0
	}
	total := n * (n - 1) / 2
	stride := max(1, total/evidenceFloorSamplePairs)
	scores := make([]float64, 0, min(evidenceFloorSamplePairs, total))
	idx := 0
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if idx%stride == 0 {
				scores = append(scores, similarity.Cosine(p.embeds[paths[i]], p.embeds[paths[j]]))
			}
			idx++
		}
	}
	if len(scores) == 0 {
		return 0
	}
	slices.Sort(scores)
	return scores[int(evidenceFloorQuantile*float64(len(scores)-1))]
}

// embeddingDependents returns the documents unusually close to a changed
// document under the live embedding model: each changed document contributes
// at most its evidenceNeighborLimit nearest neighbors, and only those that
// also clear the corpus-calibrated floor. Rank plus calibration keeps the
// sweep selective across embedding models with very different cosine
// distributions — under the old absolute threshold a dense model flagged a
// quarter of all pairs and re-analyzed nearly the whole repository.
func (p *pipeline) embeddingDependents(changed map[string]bool) map[string]bool {
	related := map[string]bool{}
	floor := p.embeddingEvidenceFloor()
	type neighbor struct {
		path  string
		score float64
	}
	for path := range changed {
		emb := p.embeds[path]
		if len(emb) == 0 {
			continue
		}
		var hits []neighbor
		for other, otherEmb := range p.embeds {
			if other == path {
				continue
			}
			if c := similarity.Cosine(emb, otherEmb); c >= floor {
				hits = append(hits, neighbor{other, c})
			}
		}
		slices.SortFunc(hits, func(a, b neighbor) int {
			if c := cmp.Compare(b.score, a.score); c != 0 {
				return c
			}
			return cmp.Compare(a.path, b.path)
		})
		for i := 0; i < len(hits) && i < evidenceNeighborLimit; i++ {
			related[hits[i].path] = true
		}
	}
	return related
}

func (p *pipeline) dependsOnChangedEvidence(doc database.Document, fm map[string]any, content string, changed, changedFm, related map[string]bool) bool {
	for _, target := range frontmatter.GraphLinkTargets(fm) {
		if changedFm[filepath.ToSlash(strings.TrimSpace(target))] {
			return true
		}
	}
	for _, mention := range p.entities.matches(doc.Path, content) {
		if changedFm[mention.path] {
			return true
		}
		for _, source := range mention.sourcePaths {
			if changedFm[source] {
				return true
			}
		}
	}
	if related[doc.Path] {
		return true
	}
	docSignature := p.sigs[doc.Path]
	if len(docSignature) == 0 {
		docSignature = similarity.Signature(content)
	}
	for path := range changed {
		if similarity.Estimate(docSignature, p.sigs[path]) >= overlapThreshold {
			return true
		}
	}
	return false
}
