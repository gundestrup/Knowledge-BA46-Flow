// The evidence machinery: what an analysis prompt actually consumed
// (recording) and which unchanged documents must be re-analyzed when that
// evidence drifts (the dependents sweep). Recorded v2 sets are exact; rows
// without a recording keep the conservative heuristic sweep.
package analyzer

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/scanner"
	"knowledge-ba46-flow/internal/similarity"
)

// analysisEvidence records which documents an analysis prompt actually
// consumed. Fm/Full distinguish candidate annotations and body-derived
// evidence; inbound and sibling paths are stored separately because they are
// path-only context. Inbound paths invalidate on set changes, not
// source-content changes; sibling paths invalidate only when a recorded
// sibling disappears — additions are projected through the semantic arms.
// ValidationContext captures the rendered local link/metadata issues.
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
func intersectsAfter(set map[string]bool, seeds map[string]int64, generation int64) bool {
	for path := range set {
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
	changed             map[string]bool   // semantic/body drift → body-level evidence
	changedFm           map[string]bool   // frontmatter drift → annotation-level evidence
	changedNewFm        map[string]bool   // never-analyzed docs → heuristic-only seeds
	analysisPending     map[string]bool   // already in the direct queue
	directPaths         map[string]bool   // direct queue membership (dependent vs direct)
	inbound             map[string]bool   // linkers to fm-changed targets (legacy arm)
	inboundNew          map[string]bool   // linkers to brand-new targets
	related             map[string]string // embedding-projection neighbors → trigger doc
	removed             map[string]int64  // missing docs → removal generation
	removedInbound      map[string]int64  // linkers to removed targets → generation
	acknowledgeRemovals bool              // real runs stamp; estimates only preview
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
	dependent, detail, skip, err := p.isEvidenceDependent(&doc, seeds)
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
	p.emit(Progress{Path: doc.Path, Outcome: "dependent", Detail: detail})
	if p.depReasons == nil {
		p.depReasons = map[string]int{}
	}
	p.depReasons[dependentCategory(detail)]++
	p.forceAnalysis[doc.Path] = true
	return !seeds.directPaths[doc.Path], nil
}

// dependentCategory reduces a sweep reason to its cause class so run summaries
// can show why the queue grew, not just that it did.
func dependentCategory(detail string) string {
	if i := strings.IndexAny(detail, ":("); i > 0 {
		return strings.TrimSpace(detail[:i])
	}
	return detail
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

// recordedEvidenceLost is the cheap first pass of the dependents sweep: does
// any recorded relied-upon or displayed evidence path now resolve to a
// changed or removed seed? Returns the firing reason or "".
func recordedEvidenceLost(fmEv, fullEv map[string]bool, seeds *sweepSeeds, seenGeneration int64, exactEvidence, recorded bool) string {
	if exactEvidence {
		if p := firstHit(fullEv, seeds.changed); p != "" {
			return "consumed evidence changed: " + p
		}
		if p := firstHitAfter(fmEv, seeds.removed, seenGeneration); p != "" {
			return "candidate removed: " + p
		}
		if p := firstHitAfter(fullEv, seeds.removed, seenGeneration); p != "" {
			return "consumed evidence removed: " + p
		}
	}
	if recorded && !exactEvidence &&
		(intersectsAfter(fmEv, seeds.removed, seenGeneration) || intersectsAfter(fullEv, seeds.removed, seenGeneration)) {
		return "recorded evidence removed"
	}
	return ""
}

// isEvidenceDependent reports whether the doc's recorded context lost or
// gained relied-upon evidence — plus a human-readable reason and trigger for
// queue attribution ("why was this doc re-analyzed"). Displayed candidates
// (Fm) are not dependencies: they invalidate only on structural loss.
func (p *pipeline) isEvidenceDependent(doc *database.Document, seeds *sweepSeeds) (dep bool, detail string, skip bool, err error) {
	ev, recorded, err := p.evidenceRecord(doc, seeds)
	if err != nil {
		return false, "", false, err
	}
	fmEv := stringSet(ev.Fm)
	fullEv := stringSet(ev.Full)
	seenGeneration := p.laneRemovalGeneration(*doc)
	exactEvidence := recorded && ev.Version >= 2
	bookkeepingOnly := false
	if detail := recordedEvidenceLost(fmEv, fullEv, seeds, seenGeneration, exactEvidence, recorded); detail != "" {
		// A changed catalog/index body recorded as consumed merge evidence is
		// bookkeeping residue — catalogs are never merge targets, and their
		// recorded presence came from shared backlink boilerplate inflating
		// the shingle overlap. Restamp instead of paying a full analysis.
		if !exactEvidence || !p.catalogOnlyConsumedDrift(detail, fullEv, seeds) {
			return true, detail, false, nil
		}
		bookkeepingOnly = true
	}
	content, err := scanner.ReadDocument(p.c, doc.Path)
	if err != nil {
		return false, "", true, nil
	}
	fm, _, err := frontmatter.Parse(content)
	if err != nil || frontmatter.NoLLMContext(fm) {
		return false, "", true, nil
	}
	if exactEvidence {
		contextChanged, detail, inboundBookkeeping, err := p.recordedContextChanged(doc, fm, content, ev)
		if err != nil {
			return false, "", false, err
		}
		if contextChanged {
			return true, detail, false, nil
		}
		bookkeepingOnly = bookkeepingOnly || inboundBookkeeping
		// New seeds and semantic projections model evidence the prompt would
		// newly cite; persisted evidence cannot contain it yet.
		if seeds.inboundNew[doc.Path] {
			return true, "new inbound reference", false, nil
		}
		if hit, detail := p.dependsOnChangedEvidence(*doc, fm, content, seeds.changed, seeds.changedNewFm, seeds.related); hit {
			return true, detail, false, nil
		}
		if err := p.restampBookkeepingOnly(doc, bookkeepingOnly, seeds); err != nil {
			return false, "", false, err
		}
		return false, "", false, nil
	}
	if recorded {
		// Older evidence flattened candidates and inbound links together.
		// Recover clear newly-added inbound paths, but do not interpret every
		// old candidate path as proof that its body was injected.
		source, err := p.legacyInboundAdded(doc.Path, fmEv)
		if err != nil {
			return false, "", false, err
		}
		if source != "" {
			return true, "new inbound reference (legacy evidence): " + source, false, nil
		}
	}
	if seeds.inbound[doc.Path] {
		return true, "inbound evidence changed", false, nil
	}
	if seeds.removedInbound[doc.Path] > seenGeneration {
		return true, "inbound reference removed", false, nil
	}
	if p := firstHitAfter(stringSet(fmLinkTargets(fm, "related", "contradicted_by", "links")), seeds.removed, seenGeneration); p != "" {
		return true, "linked document removed: " + p, false, nil
	}
	if hit, detail := p.dependsOnChangedEvidence(*doc, fm, content, seeds.changed, seeds.changedFm, seeds.related); hit {
		return true, detail, false, nil
	}
	return false, "", false, nil
}

// evidenceRecord returns the stored analysis evidence for doc, reconstructing
// and persisting an exact record for analyzed docs that predate evidence
// recording. Estimates evaluate the same reconstruction without writing.
func (p *pipeline) evidenceRecord(doc *database.Document, seeds *sweepSeeds) (analysisEvidence, bool, error) {
	ev, recorded := parseAnalysisEvidenceRecord(p.laneEvidenceJSON(*doc))
	if recorded || p.laneSHA(*doc) == "" {
		return ev, recorded, nil
	}
	fresh, err := p.restampDoc(*doc)
	if err != nil || fresh == "" {
		return ev, false, err
	}
	ev, _ = parseAnalysisEvidenceRecord(fresh)
	if seeds.acknowledgeRemovals {
		if err := p.db.RestampEvidence(p.ctx, doc.Path, p.external, fresh, p.removalGeneration); err != nil {
			return ev, true, err
		}
	}
	return ev, true, nil
}

// catalogOnlyConsumedDrift reports whether the recorded-loss detail is a
// consumed-evidence change whose every changed path is a catalog/index page.
func (p *pipeline) catalogOnlyConsumedDrift(detail string, fullEv map[string]bool, seeds *sweepSeeds) bool {
	hit := firstHit(fullEv, seeds.changed)
	return hit != "" && detail == "consumed evidence changed: "+hit && p.onlyCatalogs(fullEv, seeds.changed)
}

func (p *pipeline) legacyInboundAdded(path string, oldEvidence map[string]bool) (string, error) {
	inbound, err := p.llmVisibleInbound(path)
	if err != nil {
		return "", err
	}
	for _, source := range slices.Sorted(maps.Keys(stringSet(inbound))) {
		if !oldEvidence[source] {
			return source, nil
		}
	}
	return "", nil
}

func (p *pipeline) recordedContextChanged(doc *database.Document, fm map[string]any, content string, ev analysisEvidence) (changed bool, detail string, bookkeepingOnly bool, err error) {
	inbound, err := p.llmVisibleInbound(doc.Path)
	if err != nil {
		return false, "", false, err
	}
	if !equalStrings(ev.Inbound, inbound) {
		// A recorded inbound entry that still exists only as a body link was
		// navigational bookkeeping under an older evidence contract — never a
		// graph edge. A newly added inbound edge recorded in applied_edges is
		// a reviewed human link decision. Both are provably non-semantic
		// deltas: restamp instead of paying a full analysis (self-healing
		// migration for old records, and no echo of our own applied edits).
		added := minusStrings(inbound, ev.Inbound)
		removed := minusStrings(ev.Inbound, inbound)
		switch {
		case len(added) > 0 && len(removed) == 0:
			reviewed, err := p.inboundAdditionsAreAppliedEdges(doc.Path, added)
			if err != nil {
				return false, "", false, err
			}
			bookkeepingOnly = reviewed
		case len(added) == 0:
			bodyOnly, err := p.inboundRemovalsAreBodyLinks(doc.Path, removed)
			if err != nil {
				return false, "", false, err
			}
			bookkeepingOnly = bodyOnly
		}
		if !bookkeepingOnly {
			return true, "inbound links changed (+" + strings.Join(added, ",") +
				" -" + strings.Join(removed, ",") + ")", false, nil
		}
	}
	siblings, err := scanner.SiblingFiles(p.c, doc.Path)
	if err != nil {
		return false, "", false, err
	}
	// Only a recorded sibling disappearing is provable context loss (deleted,
	// renamed, or now-mentioned). A newly added sibling is merely a candidate
	// the prompt might newly cite — relevance, not adjacency, decides that, so
	// additions are projected through the semantic arms below instead of
	// refreshing every neighbor in the directory.
	_, siblingPaths := siblingContext(siblings, content)
	rendered := stringSet(siblingPaths)
	for _, s := range ev.Siblings {
		if !rendered[s] {
			return true, "sibling removed: " + s, false, nil
		}
	}
	if ev.ValidationContext != frontmatterContext(*doc, fm, p.knownDocs) {
		return true, "validation context changed", false, nil
	}
	return false, "", bookkeepingOnly, nil
}

// restampBookkeepingOnly rebuilds the evidence record when the sweep's only
// context delta was bookkeeping residue — a self-healing migration for
// records written under an older inbound contract, so the noise cannot
// requeue the document on every later sweep. Estimates preview the same
// verdict without writing.
func (p *pipeline) restampBookkeepingOnly(doc *database.Document, bookkeepingOnly bool, seeds *sweepSeeds) error {
	if !bookkeepingOnly {
		return nil
	}
	fresh, err := p.restampDoc(*doc)
	if err != nil {
		return err
	}
	if fresh != "" && seeds.acknowledgeRemovals {
		return p.db.RestampEvidence(p.ctx, doc.Path, p.external, fresh, p.removalGeneration)
	}
	return nil
}

// onlyCatalogs reports whether every changed document recorded in ev is a
// catalog/index page — bookkeeping consumption, never merge evidence.
func (p *pipeline) onlyCatalogs(ev map[string]bool, changed map[string]bool) bool {
	for path := range ev {
		if changed[path] && !p.catalogDocs[path] {
			return false
		}
	}
	return true
}

// inboundAdditionsAreAppliedEdges reports whether every newly inbound source
// recorded an edge via an accepted suggestion — a reviewed link decision the
// sweep should acknowledge, not re-analyze. Any unproven addition fails
// conservative and stays on the analysis path.
func (p *pipeline) inboundAdditionsAreAppliedEdges(target string, added []string) (bool, error) {
	for _, source := range added {
		reviewed, err := p.db.AppliedGraphEdgeExists(p.ctx, source, target)
		if err != nil {
			return false, err
		}
		if !reviewed {
			return false, nil
		}
	}
	return len(added) > 0, nil
}

// inboundRemovalsAreBodyLinks reports whether every recorded inbound source
// dropped from the current set still resolves to target through a body edge —
// proof the recorded entries were catalog/navigational, not graph links. An
// incomplete index or any source with no remaining edge fails conservative:
// an unprovable removal could be a real deleted `related` edge and must stay
// on the analysis path.
func (p *pipeline) inboundRemovalsAreBodyLinks(target string, removed []string) (bool, error) {
	if len(removed) == 0 {
		return false, nil
	}
	bodySources, ready, err := p.db.BodyInboundSources(p.ctx, target)
	if err != nil {
		return false, err
	}
	if !ready {
		return false, nil
	}
	body := stringSet(bodySources)
	for _, source := range removed {
		if !body[source] {
			return false, nil
		}
	}
	return true, nil
}

func equalStrings(a, b []string) bool {
	return slices.Equal(sortedUnique(a), sortedUnique(b))
}

// firstHit returns a deterministic member of set present in seeds, or "".
func firstHit(set, seeds map[string]bool) string {
	for _, p := range slices.Sorted(maps.Keys(set)) {
		if seeds[p] {
			return p
		}
	}
	return ""
}

// firstHitAfter is firstHit for generation-stamped seed maps.
func firstHitAfter(set map[string]bool, seeds map[string]int64, generation int64) string {
	for _, p := range slices.Sorted(maps.Keys(set)) {
		if seeds[p] > generation {
			return p
		}
	}
	return ""
}

// minusStrings returns the sorted members of a absent from b.
func minusStrings(a, b []string) []string {
	bs := stringSet(b)
	out := []string{}
	for _, v := range a {
		if !bs[v] {
			out = append(out, v)
		}
	}
	return sortedUnique(out)
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
func (p *pipeline) embeddingDependents(changed map[string]bool) map[string]string {
	related := map[string]string{}
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
			related[hits[i].path] = path
		}
	}
	return related
}

func (p *pipeline) dependsOnChangedEvidence(doc database.Document, fm map[string]any, content string, changed, changedFm map[string]bool, related map[string]string) (bool, string) {
	for _, target := range frontmatter.GraphLinkTargets(fm) {
		if t := filepath.ToSlash(strings.TrimSpace(target)); changedFm[t] {
			return true, "linked evidence changed: " + t
		}
	}
	for _, mention := range p.entities.matches(doc.Path, content) {
		if changedFm[mention.path] {
			return true, "entity evidence changed: " + mention.path
		}
		for _, source := range mention.sourcePaths {
			if changedFm[source] {
				return true, "entity source changed: " + source
			}
		}
	}
	if trigger, ok := related[doc.Path]; ok {
		return true, "embedding neighbor changed: " + trigger
	}
	docSignature := p.sigs[doc.Path]
	if len(docSignature) == 0 {
		docSignature = similarity.Signature(content)
	}
	for _, path := range slices.Sorted(maps.Keys(changed)) {
		if similarity.Estimate(docSignature, p.sigs[path]) >= overlapThreshold {
			return true, "content overlap: " + path
		}
	}
	return false, ""
}

// RestampEvidence rebuilds v2 evidence records for analyzed rows that lack
// them — the knowledge migration for deployments upgraded before evidence
// recording existed. Deterministic context (ranked candidates, inbound
// links, unmentioned siblings, entity paths, validation context) is
// reconstructed exactly; merge bodies are model-chosen and not
// reconstructable, so they are omitted — the scored candidates already in
// Full keep the bias toward refresh, never toward missed invalidation.
// Rows also acknowledge the current removal generation so historical
// deletions do not re-fire. No model calls.
func RestampEvidence(ctx context.Context, c config.Config, db *database.DB, opts RunOptions) (int, error) {
	models, err := resolveRunModels(c, opts)
	if err != nil {
		return 0, err
	}
	p := newPipeline(ctx, c, db, Generators{}, opts, models, false)
	if err := p.loadCorpus(); err != nil {
		return 0, err
	}
	generation, err := db.RemovalGeneration(ctx)
	if err != nil {
		return 0, err
	}
	restamped := 0
	for _, doc := range p.allDocs {
		if p.laneEvidenceJSON(doc) != "" || p.laneSHA(doc) == "" || !p.visiblePaths[doc.Path] {
			continue
		}
		ev, err := p.restampDoc(doc)
		if err != nil {
			return restamped, err
		}
		if ev == "" {
			continue
		}
		if err := db.RestampEvidence(ctx, doc.Path, p.external, ev, generation); err != nil {
			return restamped, err
		}
		restamped++
	}
	return restamped, nil
}

// restampDoc reconstructs a v2 evidence record for one analyzed document.
// Returns "" when the doc is unreadable or LLM-ineligible — callers keep
// their fallback path in that case.
func (p *pipeline) restampDoc(doc database.Document) (string, error) {
	content, err := scanner.ReadDocument(p.c, doc.Path)
	if err != nil {
		return "", nil
	}
	fm, _, err := frontmatter.Parse(content)
	if err != nil || frontmatter.NoLLMContext(fm) {
		return "", nil
	}
	docSig := p.sigs[doc.Path]
	if len(docSig) == 0 {
		docSig = similarity.Signature(content)
	}
	mentionsByPath := make(map[string][]entityMention)
	for _, mention := range p.entities.matches(doc.Path, content) {
		mentionsByPath[mention.path] = append(mentionsByPath[mention.path], mention)
	}
	ranked := rankCandidates(doc.Path, fm, visibleCandidates(doc.Path, p.allDocs, p.visiblePaths), docSig, p.sigs, p.embeds[doc.Path], p.embeds, mentionsByPath, candidateLimit)
	ranked = slices.DeleteFunc(ranked, func(c candidate) bool { return !p.currentlyVisible(c.doc.Path) || p.catalogDocs[c.doc.Path] })
	inbound, err := p.llmVisibleInbound(doc.Path)
	if err != nil {
		return "", err
	}
	siblings, err := scanner.SiblingFiles(p.c, doc.Path)
	if err != nil {
		return "", err
	}
	_, siblingPaths := siblingContext(siblings, content)
	_, entityPaths := entityEvidenceContext(p.c, ranked)
	return marshalAnalysisEvidence(ranked, sortedUnique(inbound), siblingPaths, nil, entityPaths, frontmatterContext(doc, fm, p.knownDocs)), nil
}
