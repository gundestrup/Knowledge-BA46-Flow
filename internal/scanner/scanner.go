// Package scanner walks the knowledge repository, hashes markdown files
// and keeps the documents table in sync with what is on disk.
package scanner

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/paths"
	"knowledge-ba46-flow/internal/prompts"
	"knowledge-ba46-flow/internal/similarity"
	"knowledge-ba46-flow/internal/taxonomy"
)

// Result summarises a scan: Seen = markdown files found,
// Changed = files that are new or whose sha256 differs from what SQLite has,
// IDsAssigned = files that received a new document_id this scan.
type Result struct {
	Seen        int
	Changed     int
	IDsAssigned int
}

// Scan walks c.RepositoryPath recursively, hashes every file matching
// c.Extensions minus c.ExcludeExtensions and upserts it into the documents
// table. It skips hidden entries, MCP workflow sidecars, built-in MCP
// temporary directories, configured exclude_directories and any
// repository-relative path matched by c.Exclude. Afterwards MarkMissing
// flags rows not touched by this run.
func Scan(ctx context.Context, c config.Config, db *database.DB) (Result, error) {
	// One timestamp for the whole run; MarkMissing treats 'not stamped with this
	// exact value' as 'file disappeared from disk'.
	seen := time.Now().UTC().Format(time.RFC3339Nano)

	// Lookup sets for excluded directory names and allowed extensions
	// (case-insensitive); exclude_extensions subtracts from the allowlist.
	excludes := map[string]bool{}
	for _, x := range c.ExcludeDirectories {
		excludes[x] = true
	}
	excludes[c.TrashDirectory] = true // never scan the trash
	exts := lowerSet(c.Extensions)
	for _, x := range c.ExcludeExtensions {
		delete(exts, strings.ToLower(x))
	}
	patterns, err := paths.ParseExcludePatterns(c.Exclude)
	if err != nil {
		return Result{}, err
	}
	root, err := os.OpenRoot(c.RepositoryPath)
	if err != nil {
		return Result{}, err
	}
	defer root.Close()

	taskPrompts, err := scanPromptInputs(c)
	if err != nil {
		return Result{}, err
	}
	taxonomyBytes, taxonomyExists, err := taxonomy.ReadFromRoot(root)
	if err != nil {
		return Result{}, err
	}
	taxonomySHA := ""
	var vocabulary *taxonomy.Vocabulary
	if taxonomyExists {
		taxonomySHA = fmt.Sprintf("%x", sha256.Sum256(taxonomyBytes))
		vocabulary, err = taxonomy.Parse(taxonomyBytes)
		if err != nil {
			// A malformed taxonomy cannot provide a safe scope projection.
			// Keep the whole-file digest so every affected task remains stale.
			vocabulary = nil
		}
	}
	// A parsed vocabulary lets each document fingerprint only the taxonomy
	// slice it can actually see (ScopeDigest), so unrelated vocabulary edits
	// no longer invalidate the whole corpus. A malformed file fails closed:
	// every document falls back to the whole-file digest and conservatively
	// requeues.
	w := repoWalker{
		ctx: ctx, c: c, db: db, root: root, seen: seen,
		excludes: excludes, exts: exts, exclude: patterns, paths: map[string]bool{},
		promptInputs: taskPrompts, taxonomySHA: taxonomySHA, vocabulary: vocabulary,
	}
	err = filepath.WalkDir(c.RepositoryPath, w.visit)
	if err != nil {
		return w.result, err
	}
	if err = db.MarkMissing(ctx, seen); err != nil {
		return w.result, err
	}
	if err = w.checkInternalLinks(); err != nil {
		return w.result, err
	}
	return w.result, nil
}

// repoWalker carries the scan state behind the WalkDir callback: configuration,
// lookup sets, the run timestamp and the running Result.
type repoWalker struct {
	ctx          context.Context
	c            config.Config
	db           *database.DB
	root         *os.Root
	seen         string
	excludes     map[string]bool
	exts         map[string]bool
	exclude      paths.ExcludePatterns
	result       Result
	paths        map[string]bool // every document path indexed this run
	promptInputs taskPromptInputs
	taxonomySHA  string
	vocabulary   *taxonomy.Vocabulary
}

// scopeDigest returns the per-document taxonomy fingerprint slice. Without a
// usable vocabulary it is the whole-file digest — empty for an absent file
// (matching the legacy recipe) and the file hash for a malformed one
// (fail-closed broad invalidation).
func (w *repoWalker) scopeDigest(rel string, currentTags []string) string {
	if w.vocabulary == nil {
		return w.taxonomySHA
	}
	return w.vocabulary.ScopeDigest(rel, currentTags)
}

type taskPromptInputs struct {
	analysis             string
	enrich               string
	expand               string
	freshness            string
	freshnessVerify      string
	freshnessVerifyKnown bool
}

func scanPromptInputs(c config.Config) (taskPromptInputs, error) {
	if c.PromptDirectory == "" {
		return taskPromptInputs{freshnessVerifyKnown: true}, nil
	}
	server := c.MCPServer
	if server == "" {
		server = config.MCPServerKnowledgeBase
	}
	fingerprint := func(task string, extras ...string) (string, error) {
		return prompts.Fingerprint(c.PromptDirectory, server, task, extras...)
	}
	var result taskPromptInputs
	var err error
	if result.analysis, err = fingerprint("analyze.md"); err != nil {
		return taskPromptInputs{}, err
	}
	optional := func(task string) (string, error) {
		input, err := fingerprint(task)
		if err != nil {
			return hashInputs("unavailable-optional-prompt", task), nil
		}
		return input, err
	}
	if result.enrich, err = optional("enrich.md"); err != nil {
		return taskPromptInputs{}, err
	}
	if result.expand, err = optional("expand.md"); err != nil {
		return taskPromptInputs{}, err
	}
	if result.freshness, err = optional("freshness.md"); err != nil {
		return taskPromptInputs{}, err
	}
	// Verification is an opt-in second pass. Keep its prompt out of the base
	// freshness input; RunTask fingerprints and queues that pass only when
	// --web-verify is selected. Absence is supported; other read errors fail.
	result.freshnessVerify, err = fingerprint("freshness-verify.md")
	switch {
	case err == nil:
		result.freshnessVerifyKnown = true
	case errors.Is(err, fs.ErrNotExist):
		// The prior recipe omitted an absent opt-in verifier prompt.
		result.freshnessVerifyKnown = true
	}
	return result, nil
}

func hashInputs(inputs ...string) string {
	h := sha256.New()
	for _, input := range inputs {
		_, _ = h.Write([]byte(input))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (w *repoWalker) expandTemplateHash(docPath string) (string, error) {
	dir := path.Dir(docPath)
	for {
		candidate := StubTemplateName
		if dir != "." {
			candidate = dir + "/" + StubTemplateName
		}
		content, err := w.root.ReadFile(candidate)
		if err == nil {
			return fmt.Sprintf("%x", sha256.Sum256(content)), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		if dir == "." {
			return "", nil
		}
		parent := path.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}

// visit handles one walk entry: prune unwanted trees, skip non-documents and
// index files that survive the filters.
func (w *repoWalker) visit(path string, e os.DirEntry, walkErr error) error {
	if walkErr != nil {
		return walkErr
	}
	isRoot := path == w.c.RepositoryPath
	if !isRoot && strings.HasPrefix(e.Name(), ".") {
		if e.IsDir() {
			return filepath.SkipDir
		}
		return nil
	}
	// Do not hash links to content outside the repository. ReadDocument
	// independently confines reads with os.Root as defense in depth.
	if e.Type()&os.ModeSymlink != 0 {
		return nil
	}
	rel, err := w.relativePath(path, isRoot)
	if err != nil {
		return err
	}
	// Prune excluded directories (the root itself is never pruned).
	if e.IsDir() {
		if !isRoot && (w.excludes[e.Name()] || MCPExcludedDirectory(rel) || w.exclude.Match(rel)) {
			return filepath.SkipDir
		}
		return nil
	}
	if MCPExcludedPath(rel) || w.exclude.Match(rel) {
		return nil
	}
	// Stub expansion templates are authoring scaffolding, not knowledge
	// documents — they never become corpus entries or link targets.
	if e.Name() == StubTemplateName {
		return nil
	}
	// Skip files outside the configured extension set.
	if !w.exts[strings.ToLower(filepath.Ext(e.Name()))] {
		return nil
	}
	return w.indexFile(path, rel, e)
}

// relativePath returns the forward-slash repo-relative path; the root itself
// maps to "".
func (w *repoWalker) relativePath(path string, isRoot bool) (string, error) {
	if isRoot {
		return "", nil
	}
	rel, err := filepath.Rel(w.c.RepositoryPath, path)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// indexFile hashes one document, curates its frontmatter and upserts the row.
func (w *repoWalker) indexFile(path, rel string, e os.DirEntry) error {
	// Store paths repo-relative, always with forward slashes for cross-platform stability.
	info, err := e.Info()
	if err != nil {
		return err
	}
	local, err := paths.CleanRelative(rel)
	if err != nil {
		return err
	}
	raw, err := w.root.ReadFile(local)
	if err != nil {
		return err
	}
	// Identity assignment is a tool-owned, hash-neutral frontmatter write: it
	// runs before hashing so the stored parse always reflects the file on disk.
	content, idIssue := w.ensureDocumentID(rel, local, string(raw))
	hashes, sig, err := hashAndSignatureContent(content)
	if err != nil {
		return err
	}
	fields, body, frontmatterErr := frontmatter.Parse(content)
	stubState := "stub=unparseable"
	var currentTags []string
	var analysisTaxonomySHA, enrichmentTaxonomySHA, expandTaxonomySHA string
	private := false
	if frontmatterErr == nil {
		private = frontmatter.NoLLMContext(fields)
		isStub := frontmatter.IsStub(fields, body, w.c.StubLineLimit())
		if private {
			stubState = "generation=private"
		} else {
			stubState = fmt.Sprintf("stub=%t", isStub)
		}
		if w.vocabulary != nil {
			currentTags = frontmatter.StringList(fields, "tags")
		}
		if !private {
			scopeSHA := w.scopeDigest(rel, currentTags)
			if isStub {
				expandTaxonomySHA = scopeSHA
			} else {
				analysisTaxonomySHA = scopeSHA
				enrichmentTaxonomySHA = scopeSHA
			}
		}
	}
	// Preserve the old recipe for safe lineage migration. The current recipe
	// only depends on templates for actual stubs.
	legacyTemplateSHA, err := w.expandTemplateHash(rel)
	if err != nil {
		return err
	}
	templateSHA := ""
	if stubState == "stub=true" && !private {
		templateSHA = legacyTemplateSHA
	}
	// Path-derived tags, e.g. frameworks/bulma/x.md -> ["frameworks","bulma","x"].
	tags := pathTags(rel)
	b, _ := json.Marshal(tags)
	// Local frontmatter curation: parse + validate against the
	// knowledge-base-mcp-server whitelist; issues are stored, not fatal.
	fmJSON, fmIssues, err := inspectContent(content)
	if err != nil {
		return err
	}
	if idIssue != "" {
		fmIssues = appendIssue(fmIssues, idIssue)
	}
	legacyStubLines := fmt.Sprintf("stub_max_body_lines=%d", w.c.StubLineLimit())
	legacyFreshnessPrompt := w.promptInputs.freshness
	if w.promptInputs.freshnessVerify != "" {
		legacyFreshnessPrompt = hashInputs(legacyFreshnessPrompt, w.promptInputs.freshnessVerify)
	}
	legacyFreshnessInput := ""
	if w.promptInputs.freshnessVerifyKnown {
		legacyFreshnessInput = hashInputs(hashes.semantic, legacyFreshnessPrompt, legacyStubLines)
	}
	// UpsertScanned reports any strict source change. It requeues tasks only
	// when the semantic input changed, while stale full-file suggestions are
	// superseded for every strict source change.
	changed, err := w.db.UpsertScanned(w.ctx, database.Document{
		Path:                   rel,
		SHA256:                 hashes.combined,
		FmSHA256:               hashes.frontmatter,
		BodySHA256:             hashes.body,
		SemanticSHA256:         hashes.semantic,
		SemanticFrontmatterSHA: hashes.semanticFrontmatter,
		SemanticBodySHA256:     hashes.semanticBody,
		AnalysisInputSHA256: hashInputs(
			hashes.semantic, w.promptInputs.analysis, analysisTaxonomySHA,
			"write_scope="+w.c.EffectiveWriteScope(), stubState),
		EnrichmentInputSHA256: hashInputs(
			hashes.semantic, w.promptInputs.enrich, enrichmentTaxonomySHA,
			"enrichment_mode="+string(w.c.EnrichmentMode), stubState),
		ExpandInputSHA256: hashInputs(
			hashes.semantic, w.promptInputs.expand, templateSHA, expandTaxonomySHA, stubState),
		// Legacy fingerprints recompute the previous whole-file recipe so
		// UpsertScanned can re-stamp lineages proven current under it.
		LegacyAnalysisInputSHA256: hashInputs(
			hashes.semantic, w.promptInputs.analysis, w.taxonomySHA,
			"write_scope="+w.c.EffectiveWriteScope(), legacyStubLines),
		LegacyEnrichmentInputSHA256: hashInputs(
			hashes.semantic, w.promptInputs.enrich, w.taxonomySHA,
			"enrichment_mode="+string(w.c.EnrichmentMode), legacyStubLines),
		LegacyExpandInputSHA256: hashInputs(
			hashes.semantic, w.promptInputs.expand, legacyTemplateSHA, w.taxonomySHA, legacyStubLines),
		LegacyFreshnessInputSHA256: legacyFreshnessInput,
		FreshnessInputSHA256: hashInputs(
			hashes.semantic, w.promptInputs.freshness,
			stubState),
		Size:              info.Size(),
		ModifiedUnix:      info.ModTime().Unix(),
		TagsJSON:          string(b),
		FrontmatterJSON:   fmJSON,
		FrontmatterIssues: fmIssues,
		Signature:         similarity.Marshal(sig),
	}, w.seen, func(previous string) bool {
		local, err := paths.CleanRelative(previous)
		if err != nil {
			return false
		}
		_, err = w.root.Lstat(local)
		return os.IsNotExist(err)
	})
	if err != nil {
		return err
	}
	w.result.Seen++
	w.paths[rel] = true
	if changed {
		w.result.Changed++
	}
	return nil
}

// lowerSet builds a case-insensitive membership set from values.
func lowerSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, v := range values {
		out[strings.ToLower(v)] = true
	}
	return out
}

// StubTemplateName is the reserved filename for per-category stub expansion
// templates. The nearest STUB.md walking up from a stub's directory (to the
// repository root) is the skeleton the expand task fills in; template files
// are never scanned as documents and are exempt from structural validation.
const StubTemplateName = "STUB.md"

var (
	mcpExcludedFirstDirectories = map[string]bool{
		"logs": true, "tmp": true, "_tmp": true,
	}
	mcpExcludedPathSegments = map[string]bool{
		"_seen.jsonl": true, "_seen.json": true, "_index.jsonl": true,
	}
)

// MCPExcludedDirectory mirrors the MCP ingest filter's first-shelf-directory
// exclusions and sidecar path handling. Dot-prefixed directories are handled
// directly by Scan so nested hidden content is pruned before traversal.
func MCPExcludedDirectory(relative string) bool {
	parts := strings.Split(filepath.ToSlash(relative), "/")
	if len(parts) == 0 {
		return false
	}
	if mcpExcludedFirstDirectories[parts[0]] {
		return true
	}
	for _, part := range parts {
		if mcpExcludedPathSegments[part] {
			return true
		}
	}
	return false
}

// MCPExcludedPath mirrors MCP's non-ingestable sidecars. The logs/tmp rules
// exclude their subtree only when it is the first path component.
func MCPExcludedPath(relative string) bool {
	parts := strings.Split(filepath.ToSlash(relative), "/")
	if len(parts) > 1 && mcpExcludedFirstDirectories[parts[0]] {
		return true
	}
	for _, part := range parts {
		if mcpExcludedPathSegments[part] {
			return true
		}
	}
	base := filepath.Base(relative)
	lowerBase := strings.ToLower(base)
	return strings.HasPrefix(base, "._") ||
		base == ".DS_Store" ||
		lowerBase == "thumbs.db" ||
		base == "desktop.ini" ||
		strings.HasSuffix(strings.ToLower(base), ":zone.identifier")
}

// inspectContent is inspect on already-read content — indexFile has the file
// in memory after the identity step, so re-reading is pointless.
func inspectContent(raw string) (string, string, error) {
	if len(raw) > 16384 {
		raw = raw[:16384]
	}
	fields, _, perr := frontmatter.Parse(raw)
	if perr != nil {
		issues, _ := json.Marshal([]string{perr.Error()})
		return "", string(issues), nil
	}
	if fields == nil {
		return "", "[]", nil
	}
	fmJSON, _ := json.Marshal(fields)
	issues, _ := json.Marshal(frontmatter.Validate(fields))
	return string(fmJSON), string(issues), nil
}

// canonicalHashes keeps strict source hashes separate from semantic work
// fingerprints. The full canonical hash protects reviewed proposals; semantic
// hashes ignore graph-only links and frontmatter ordering for task selection.
type canonicalHashes struct {
	combined            string
	frontmatter         string
	body                string
	semantic            string
	semanticFrontmatter string
	semanticBody        string
}

// hashAndSignature reads the file once and computes both the strict source
// hash and the semantic fingerprint used for task selection. The raw
// frontmatter/body hashes remain available for reporting; similarity uses the
// semantic projection so graph-only links do not distort relatedness.
func hashAndSignature(path string) (canonicalHashes, []uint64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return canonicalHashes{}, nil, err
	}
	return hashAndSignatureContent(string(raw))
}

func hashAndSignatureContent(raw string) (canonicalHashes, []uint64, error) {
	fm, body := frontmatter.CanonicalParts(raw)
	content := fm + body
	semanticFM, semanticBody := frontmatter.SemanticParts(raw)
	semanticContent := semanticFM + "\x00" + semanticBody
	h := sha256.Sum256([]byte(content))
	sk := similarity.NewSketcher()
	words := bufio.NewScanner(strings.NewReader(semanticContent))
	words.Split(bufio.ScanWords)
	words.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for words.Scan() {
		sk.AddWord(words.Text())
	}
	if err := words.Err(); err != nil {
		return canonicalHashes{}, nil, err
	}
	return canonicalHashes{
		combined:            hex.EncodeToString(h[:]),
		frontmatter:         fmt.Sprintf("%x", sha256.Sum256([]byte(fm))),
		body:                fmt.Sprintf("%x", sha256.Sum256([]byte(body))),
		semantic:            fmt.Sprintf("%x", sha256.Sum256([]byte(semanticContent))),
		semanticFrontmatter: fmt.Sprintf("%x", sha256.Sum256([]byte(semanticFM))),
		semanticBody:        fmt.Sprintf("%x", sha256.Sum256([]byte(semanticBody))),
	}, sk.Signature(), nil
}

// CanonicalHash returns the strict source hash used by review/apply freshness
// checks. It intentionally includes graph links and formatting but excludes
// only the tool-owned kbm block and document_id.
func CanonicalHash(content string) string {
	canonical := frontmatter.Canonical(content)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(canonical)))
}

// pathTags derives tags from the relative path: every directory component plus the
// filename stem, lowercased, with '_' and ' ' normalised to '-'. "readme" and
// duplicates are dropped. frameworks/bulma/theme_controller.md ->
// ["frameworks","bulma","theme-controller"].
func pathTags(rel string) []string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	seen := map[string]bool{}
	var tags []string
	for i, p := range parts {
		if i == len(parts)-1 {
			p = strings.TrimSuffix(p, filepath.Ext(p)) // filename -> stem
		}
		p = strings.ToLower(strings.TrimSpace(p))
		p = strings.ReplaceAll(p, "_", "-")
		p = strings.ReplaceAll(p, " ", "-")
		if p != "" && p != "readme" && !seen[p] {
			seen[p] = true
			tags = append(tags, p)
		}
	}
	return tags
}

// ReadDocument loads a document by repo-relative path and enforces
// c.MaxDocumentBytes so an oversized file can never reach the prompt.
func ReadDocument(c config.Config, rel string) (string, error) {
	localPath, err := paths.CleanRelative(rel)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(c.RepositoryPath)
	if err != nil {
		return "", err
	}
	defer root.Close()
	f, err := root.Open(localPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	maxBytes := c.MaxDocumentBytes
	if maxBytes <= 0 {
		maxBytes = 200000
	}
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(b)) > maxBytes {
		return "", fmt.Errorf("document exceeds max_document_bytes (%d)", maxBytes)
	}
	return string(b), nil
}

// SiblingFiles returns the names of regular files sharing the document's
// directory that are not scanned documents — helper scripts, configs or data
// files the document may need to reference. Directories, hidden files and the
// document itself are excluded. Reads stay confined by os.Root.
func SiblingFiles(c config.Config, rel string) ([]string, error) {
	local, err := paths.CleanRelative(rel)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(c.RepositoryPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), filepath.ToSlash(filepath.Dir(local)))
	if err != nil {
		return nil, err
	}
	docExts := map[string]bool{}
	for _, x := range c.Extensions {
		docExts[strings.ToLower(x)] = true
	}
	excludedExts := map[string]bool{}
	for _, x := range c.ExcludeExtensions {
		excludedExts[strings.ToLower(x)] = true
	}
	patterns, err := paths.ParseExcludePatterns(c.Exclude)
	if err != nil {
		return nil, err
	}
	dir := filepath.ToSlash(filepath.Dir(local))
	out := []string{}
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || strings.HasPrefix(name, ".") || name == filepath.Base(local) {
			continue
		}
		ext := strings.ToLower(filepath.Ext(name))
		if docExts[ext] || excludedExts[ext] {
			continue
		}
		sib := name
		if dir != "." {
			sib = dir + "/" + name
		}
		if patterns.Match(sib) {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}

// cleanRelative rejects absolute paths, the repository root itself, and any
// lexical traversal before a path is passed to an os.Root operation.
