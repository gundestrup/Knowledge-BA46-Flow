// Package validate implements the read-only `KBM-flow validate` structural
// gate: the repository conventions documented in the shelf's README — every
// in-scope note carries taxonomy-valid frontmatter tags, every note links
// its index README, every index lists its descendant notes, language notes
// link their catalogs, and every snippet/asset inside a note tree is
// referenced by name from a note. No files are mutated.
package validate

import (
	"context"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/paths"
	"knowledge-ba46-flow/internal/scanner"
	"knowledge-ba46-flow/internal/taxonomy"
)

// Run walks the repository and returns every structural violation found,
// sorted. The vocabulary may be nil (no taxonomy.yaml): vocabulary checks
// are then skipped while structural link checks still run.
func Run(ctx context.Context, c config.Config, vocabulary *taxonomy.Vocabulary) ([]string, error) {
	scope, err := newScope(c)
	if err != nil {
		return nil, err
	}
	inventory, err := scope.collect(ctx, c)
	if err != nil {
		return nil, err
	}
	bodies, violations := scope.readBodies(c, inventory)
	violations = append(violations, scope.checkNotes(inventory, bodies, vocabulary)...)
	violations = append(violations, scope.checkCatalogEdges(inventory, bodies)...)
	violations = append(violations, scope.checkOrphans(inventory, bodies)...)
	sort.Strings(violations)
	return violations, nil
}

// scope carries the walk scoping rules: scanner exclusions plus the
// validate.exempt list that marks meta/scratch paths as outside knowledge
// scope (no tag, index, or link requirements).
type scope struct {
	excludes paths.ExcludePatterns
	exempt   paths.ExcludePatterns
	exts     map[string]bool
	dirNames map[string]bool
}

func newScope(c config.Config) (*scope, error) {
	excludes, err := paths.ParseExcludePatterns(c.Exclude)
	if err != nil {
		return nil, err
	}
	exempt, err := paths.ParseExcludePatterns(c.Validate.Exempt)
	if err != nil {
		return nil, err
	}
	s := &scope{excludes: excludes, exempt: exempt, exts: map[string]bool{}, dirNames: map[string]bool{}}
	for _, e := range c.Extensions {
		s.exts[strings.ToLower(e)] = true
	}
	for _, d := range c.ExcludeDirectories {
		s.dirNames[strings.ToLower(d)] = true
	}
	return s, nil
}

// inScope reports whether rel is inside knowledge scope: neither it nor any
// ancestor directory may match a validate.exempt pattern.
func (s *scope) inScope(rel string) bool {
	for d := rel; d != "." && d != ""; d = path.Dir(d) {
		if s.exempt.Match(d) {
			return false
		}
	}
	return true
}

// inventory is the classified repository listing.
type inventory struct {
	notes   []string // non-README notes with a scanned extension
	readmes []string // README.md index files
	others  []string // non-Markdown files eligible for the orphan check
}

func (s *scope) collect(ctx context.Context, c config.Config) (inventory, error) {
	var inv inventory
	err := filepath.WalkDir(c.RepositoryPath, func(fpath string, e fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		isRoot := fpath == c.RepositoryPath
		if !isRoot && strings.HasPrefix(e.Name(), ".") {
			if e.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if e.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		rel, rerr := filepath.Rel(c.RepositoryPath, fpath)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if e.IsDir() {
			if !isRoot && (s.dirNames[strings.ToLower(e.Name())] || scanner.MCPExcludedDirectory(rel) || s.excludes.Match(rel)) {
				return filepath.SkipDir
			}
			return nil
		}
		if e.Type()&fs.ModeType != 0 || scanner.MCPExcludedPath(rel) || s.excludes.Match(rel) {
			return nil
		}
		// Stub expansion templates are authoring scaffolding, not notes.
		if e.Name() == scanner.StubTemplateName {
			return nil
		}
		if s.exts[strings.ToLower(filepath.Ext(e.Name()))] {
			if filepath.Base(rel) == "README.md" {
				inv.readmes = append(inv.readmes, rel)
			} else {
				inv.notes = append(inv.notes, rel)
			}
		} else if filepath.Dir(rel) != "." && s.inScope(rel) {
			inv.others = append(inv.others, rel)
		}
		return nil
	})
	return inv, err
}

// readBodies loads every note once (os.Root-confined, size-capped).
// Unreadable notes become violations rather than aborting the gate.
func (s *scope) readBodies(c config.Config, inv inventory) (map[string]string, []string) {
	bodies := map[string]string{}
	var violations []string
	for _, rel := range append(append([]string{}, inv.notes...), inv.readmes...) {
		body, err := scanner.ReadDocument(c, rel)
		if err != nil {
			violations = append(violations, rel+": unreadable: "+err.Error())
			continue
		}
		bodies[rel] = body
	}
	return bodies, violations
}

// indexFor returns the nearest ancestor README.md, or false when none exists.
func indexFor(readmeSet map[string]bool, rel string) (string, bool) {
	for dir := path.Dir(rel); ; dir = path.Dir(dir) {
		candidate := "README.md"
		if dir != "." {
			candidate = dir + "/README.md"
		}
		if readmeSet[candidate] {
			return candidate, true
		}
		if dir == "." {
			return "", false
		}
	}
}

// checkNotes verifies each in-scope note: valid frontmatter tags per the
// taxonomy, an index back-link, and listing by its nearest index.
func (s *scope) checkNotes(inv inventory, bodies map[string]string, vocabulary *taxonomy.Vocabulary) []string {
	readmeSet := map[string]bool{}
	for _, r := range inv.readmes {
		readmeSet[r] = true
	}
	var violations []string
	for _, rel := range inv.notes {
		if !s.inScope(rel) {
			continue
		}
		body, ok := bodies[rel]
		if !ok {
			continue
		}
		fields, _, perr := frontmatter.Parse(body)
		if perr != nil {
			violations = append(violations, rel+": "+perr.Error())
		} else {
			tags := frontmatter.StringList(fields, "tags")
			if len(tags) == 0 {
				violations = append(violations, rel+": missing frontmatter tags list")
			}
			for _, issue := range vocabulary.InvalidTagsForPath(rel, tags) {
				violations = append(violations, rel+": "+issue)
			}
		}
		if !strings.Contains(body, "README.md") {
			violations = append(violations, rel+": no index back-link (no mention of README.md)")
		}
		if idx, found := indexFor(readmeSet, rel); !found {
			violations = append(violations, rel+": no index README in any ancestor directory")
		} else if s.inScope(idx) && !strings.Contains(bodies[idx], filepath.Base(rel)) {
			violations = append(violations, idx+": does not list "+filepath.Base(rel))
		}
	}
	return violations
}

// checkCatalogEdges verifies language↔catalog edges: entries and indexes
// under libraries/<lang>/ and frameworks/<lang>/ link languages/<lang>.md,
// and the language note links each existing catalog README.
func (s *scope) checkCatalogEdges(inv inventory, bodies map[string]string) []string {
	readmeSet := map[string]bool{}
	for _, r := range inv.readmes {
		readmeSet[r] = true
	}
	var violations []string
	for _, rel := range append(append([]string{}, inv.notes...), inv.readmes...) {
		if !s.inScope(rel) {
			continue
		}
		body := bodies[rel]
		parts := strings.Split(path.Dir(rel), "/")
		if len(parts) == 2 && (parts[0] == "libraries" || parts[0] == "frameworks") {
			langNote := "languages/" + parts[1] + ".md"
			if _, exists := bodies[langNote]; exists &&
				!strings.Contains(body, langNote) && !strings.Contains(body, parts[1]+".md") {
				violations = append(violations, rel+": does not link language note "+langNote)
			}
			continue
		}
		if path.Dir(rel) != "languages" || filepath.Base(rel) == "README.md" {
			continue
		}
		lang := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
		for _, kind := range []string{"libraries", "frameworks"} {
			catalog := kind + "/" + lang + "/README.md"
			if readmeSet[catalog] && !strings.Contains(body, catalog) && !strings.Contains(body, lang+"/README.md") {
				violations = append(violations, rel+": does not link catalog "+catalog)
			}
		}
	}
	return violations
}

// checkOrphans enforces the snippet rule: a non-Markdown file inside a tree
// that contains in-scope notes must be named in some note. Files at root or
// under note-free trees are repo tooling and exempt.
func (s *scope) checkOrphans(inv inventory, bodies map[string]string) []string {
	dirsWithNotes := map[string]bool{}
	for _, rel := range inv.notes {
		if !s.inScope(rel) {
			continue
		}
		for dir := path.Dir(rel); dir != "."; dir = path.Dir(dir) {
			dirsWithNotes[dir] = true
		}
	}
	var linked strings.Builder
	for _, body := range bodies {
		linked.WriteString(body)
		linked.WriteByte('\n')
	}
	linkedText := linked.String()
	var violations []string
	for _, rel := range inv.others {
		for d := path.Dir(rel); d != "."; d = path.Dir(d) {
			if dirsWithNotes[d] {
				if !strings.Contains(linkedText, path.Base(rel)) {
					violations = append(violations, rel+": non-Markdown file not referenced by name from any note")
				}
				break
			}
		}
	}
	return violations
}
