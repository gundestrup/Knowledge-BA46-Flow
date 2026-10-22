// Package scanner walks the knowledge repository, hashes markdown files
// and keeps the documents table in sync with what is on disk.
package scanner

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/links"
	"knowledge-ba46-flow/internal/paths"
	"knowledge-ba46-flow/internal/similarity"
)

// Result summarises a scan: Seen = markdown files found,
// Changed = files that are new or whose sha256 differs from what SQLite has.
type Result struct {
	Seen    int
	Changed int
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

	w := repoWalker{ctx: ctx, c: c, db: db, seen: seen, excludes: excludes, exts: exts, exclude: patterns, paths: map[string]bool{}}
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
	ctx      context.Context
	c        config.Config
	db       *database.DB
	seen     string
	excludes map[string]bool
	exts     map[string]bool
	exclude  paths.ExcludePatterns
	result   Result
	paths    map[string]bool // every document path indexed this run
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
	hashes, sig, err := hashAndSignature(path)
	if err != nil {
		return err
	}
	// Path-derived tags, e.g. frameworks/bulma/x.md -> ["frameworks","bulma","x"].
	tags := pathTags(rel)
	b, _ := json.Marshal(tags)
	// Local frontmatter curation: parse + validate against the
	// knowledge-base-mcp-server whitelist; issues are stored, not fatal.
	fmJSON, fmIssues, err := inspect(path)
	if err != nil {
		return err
	}
	// UpsertScanned reports whether the row is new or the hash changed; a change
	// also resets status to 'pending' and supersedes stale suggestions.
	changed, err := w.db.UpsertScanned(w.ctx, database.Document{
		Path:              rel,
		SHA256:            hashes.combined,
		FmSHA256:          hashes.frontmatter,
		BodySHA256:        hashes.body,
		Size:              info.Size(),
		ModifiedUnix:      info.ModTime().Unix(),
		TagsJSON:          string(b),
		FrontmatterJSON:   fmJSON,
		FrontmatterIssues: fmIssues,
		Signature:         similarity.Marshal(sig),
	}, w.seen)
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

// checkInternalLinks is the mandatory, always-local link check: for every
// document it resolves markdown body link targets against the scanned
// document set (markdown targets) or the repository filesystem (asset
// targets) and stores the findings as a JSON array in link_issues. Nothing
// here mutates files or needs a model.
func (w *repoWalker) checkInternalLinks() error {
	root, err := os.OpenRoot(w.c.RepositoryPath)
	if err != nil {
		return err
	}
	defer root.Close()
	for rel := range w.paths {
		raw, err := os.ReadFile(filepath.Join(w.c.RepositoryPath, filepath.FromSlash(rel)))
		if err != nil {
			continue // race: file removed between walk and check — next scan catches it
		}
		_, body, err := frontmatter.Parse(string(raw))
		if err != nil {
			continue // parse failure is already a stored frontmatter issue
		}
		issues := []string{} // non-nil so the stored JSON is [] rather than null
		for _, target := range links.InternalTargets(body) {
			resolved := false
			for _, candidate := range links.InternalCandidates(rel, target) {
				if links.IsDocumentTarget(target) {
					if w.paths[candidate] {
						resolved = true
						break
					}
				} else if local, err := paths.CleanRelative(candidate); err == nil {
					if _, err := root.Stat(local); err == nil {
						resolved = true
						break
					}
				}
			}
			if !resolved {
				issues = append(issues, "unresolved link target: "+target)
			}
		}
		j, _ := json.Marshal(issues)
		if err := w.db.SetLinkIssues(w.ctx, rel, string(j)); err != nil {
			return err
		}
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

// inspect reads the top of a file, parses its YAML frontmatter and validates it
// against the frontmatter whitelist. Return values are JSON encodings for the
// documents table: ” when the file has no frontmatter, '[]' when no issues.
// A malformed block (unclosed fence, bad YAML) is reported as one issue.
func inspect(path string) (string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	// Frontmatter must close within the first 8 KiB; 16 KiB covers every valid
	// block with room to spare and bounds the read for large files.
	b, err := io.ReadAll(io.LimitReader(f, 16384))
	if err != nil {
		return "", "", err
	}
	fields, _, perr := frontmatter.Parse(string(b))
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

// canonicalHashes holds the semantic hash split at the frontmatter boundary:
// combined is the canonical document hash (fm+body), the halves let tasks and
// the UI tell metadata-only edits from content edits.
type canonicalHashes struct {
	combined    string
	frontmatter string
	body        string
}

// hashAndSignature reads the file once and hashes the canonical content —
// the document with the tool-owned `kbm` frontmatter block excised — so
// updating check metadata never requeues analysis. The frontmatter and body
// halves hash independently, and the similarity sketch runs over the same
// canonical text, keeping managed tokens out of the duplicate signal.
func hashAndSignature(path string) (canonicalHashes, []uint64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return canonicalHashes{}, nil, err
	}
	fm, body := frontmatter.CanonicalParts(string(raw))
	content := fm + body
	h := sha256.New()
	sk := similarity.NewSketcher()
	words := bufio.NewScanner(io.TeeReader(strings.NewReader(content), h))
	words.Split(bufio.ScanWords)
	words.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for words.Scan() {
		sk.AddWord(words.Text())
	}
	if err := words.Err(); err != nil {
		return canonicalHashes{}, nil, err
	}
	return canonicalHashes{
		combined:    hex.EncodeToString(h.Sum(nil)),
		frontmatter: fmt.Sprintf("%x", sha256.Sum256([]byte(fm))),
		body:        fmt.Sprintf("%x", sha256.Sum256([]byte(body))),
	}, sk.Signature(), nil
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
	b, err := io.ReadAll(io.LimitReader(f, c.MaxDocumentBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(b)) > c.MaxDocumentBytes {
		return "", fmt.Errorf("document exceeds max_document_bytes (%d)", c.MaxDocumentBytes)
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
