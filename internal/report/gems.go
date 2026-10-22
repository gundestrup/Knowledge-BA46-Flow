package report

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var (
	lockSpecLine = regexp.MustCompile(`^    (\S+) \(([^)\s]+)\)`)
	lockDepLine  = regexp.MustCompile(`^      (\S+)(?: \(([^)]*)\))?`)
	lockRootDep  = regexp.MustCompile(`^  (\S+)`)
	gemspecDep   = regexp.MustCompile(`add_(runtime_|development_)?dependency\s*\(?\s*(.+)`)
	gemspecArg   = regexp.MustCompile(`["']([^"']+)["']`)
	gemSkipDirs  = map[string]bool{
		"vendor": true, "node_modules": true, ".git": true, "_site": true,
		"tmp": true, "coverage": true, "pkg": true,
	}
)

type parsedLockfile struct {
	Specs  map[string]string
	Edges  map[string][]GemEdge
	Direct []string
}

type gemInventoryData struct {
	Versions      map[string]map[string][]string
	FirstVersion  map[string]string
	Edges         map[string][]GemEdge
	Direct        []string
	GemspecDeps   map[string]GemspecDependency
	HasGemfile    bool
	HasGemspec    bool
	LockfilePaths []string
}

func collectGemInventory(dir string) gemInventoryData {
	locks := matchingRepoFiles(dir, "Gemfile.lock")
	gemfiles := matchingRepoFiles(dir, "Gemfile")
	gemspecs, _ := filepath.Glob(filepath.Join(dir, "*.gemspec"))
	gemspecs = filterGemFiles(dir, gemspecs)
	slices.Sort(locks)
	slices.Sort(gemfiles)
	slices.Sort(gemspecs)

	data := gemInventoryData{
		Versions:      map[string]map[string][]string{},
		FirstVersion:  map[string]string{},
		Edges:         map[string][]GemEdge{},
		GemspecDeps:   map[string]GemspecDependency{},
		HasGemfile:    len(gemfiles) > 0,
		HasGemspec:    len(gemspecs) > 0,
		LockfilePaths: locks,
	}
	for _, path := range locks {
		mergeLockfileData(&data, dir, path, parseLockfile(path))
	}
	data.Direct = uniqueSorted(data.Direct)
	for _, path := range gemspecs {
		for name, dep := range parseGemspec(path) {
			data.GemspecDeps[name] = dep
		}
	}
	return data
}

// mergeLockfileData folds one parsed Gemfile.lock into the inventory: spec
// versions keyed by repo-relative lockfile path, dependency edges stamped with
// that same source, and the lockfile's direct dependency list.
func mergeLockfileData(data *gemInventoryData, dir, path string, parsed parsedLockfile) {
	rel := repoRelativePath(dir, path)
	for name, version := range parsed.Specs {
		if data.Versions[name] == nil {
			data.Versions[name] = map[string][]string{}
		}
		if _, exists := data.FirstVersion[name]; !exists {
			data.FirstVersion[name] = version
		}
		data.Versions[name][version] = appendUnique(data.Versions[name][version], rel)
	}
	for name, edges := range parsed.Edges {
		for _, edge := range edges {
			edge.Src = rel
			data.Edges[name] = appendUnique(data.Edges[name], edge)
		}
	}
	data.Direct = append(data.Direct, parsed.Direct...)
}

// repoRelativePath renders path relative to root with forward slashes, falling
// back to the raw path when it cannot be relativised.
func repoRelativePath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	return filepath.ToSlash(rel)
}

func parseLockfile(path string) parsedLockfile {
	result := parsedLockfile{
		Specs: map[string]string{},
		Edges: map[string][]GemEdge{},
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return result
	}
	section := ""
	current := ""
	for _, line := range strings.Split(string(content), "\n") {
		if next, boundary := lockfileSection(line); boundary {
			section = next
			current = ""
			continue
		}
		switch section {
		case "specs":
			current = parseSpecsLine(&result, current, line)
		case "dependencies":
			if match := lockRootDep.FindStringSubmatch(line); match != nil {
				result.Direct = append(result.Direct, strings.TrimSuffix(match[1], "!"))
			}
		}
	}
	result.Direct = uniqueSorted(result.Direct)
	return result
}

// lockfileSection detects Gemfile.lock section boundaries: '  specs:' opens a
// spec block, DEPENDENCIES the direct-dependency list, and any other
// non-indented line starts an irrelevant section.
func lockfileSection(line string) (section string, boundary bool) {
	switch {
	case strings.HasPrefix(line, "  specs:"):
		return "specs", true
	case line == "DEPENDENCIES":
		return "dependencies", true
	case len(line) > 0 && line[0] != ' ':
		return "", true
	}
	return "", false
}

// parseSpecsLine handles one line inside a specs: block — a spec line records
// the gem version and becomes the new current spec; a deeper-indented
// dependency line records an edge on the current spec.
func parseSpecsLine(result *parsedLockfile, current, line string) string {
	if match := lockSpecLine.FindStringSubmatch(line); match != nil {
		result.Specs[match[1]] = normalizeLockVersion(match[2])
		return match[1]
	}
	if current != "" {
		if match := lockDepLine.FindStringSubmatch(line); match != nil {
			result.Edges[match[1]] = append(result.Edges[match[1]], GemEdge{
				By: current, Req: match[2],
			})
		}
	}
	return current
}

func normalizeLockVersion(version string) string {
	if platform := strings.IndexByte(version, '-'); platform >= 0 {
		return version[:platform]
	}
	return version
}

func parseGemspec(path string) map[string]GemspecDependency {
	out := map[string]GemspecDependency{}
	content, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(content), "\n") {
		match := gemspecDep.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		args := gemspecArg.FindAllStringSubmatch(match[2], -1)
		if len(args) == 0 {
			continue
		}
		dep := GemspecDependency{Dev: strings.HasPrefix(match[1], "development")}
		if len(args) > 1 {
			dep.Req = strings.Join(func() []string {
				values := make([]string, 0, len(args)-1)
				for _, arg := range args[1:] {
					values = append(values, arg[1])
				}
				return values
			}(), ", ")
		}
		out[args[0][1]] = dep
	}
	return out
}

func matchingRepoFiles(root, filename string) []string {
	patterns := []string{
		filepath.Join(root, filename),
		filepath.Join(root, "*", filename),
		filepath.Join(root, "*", "*", filename),
	}
	var out []string
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		out = append(out, filterGemFiles(root, matches)...)
	}
	return uniqueSorted(out)
}

func filterGemFiles(root string, files []string) []string {
	out := make([]string, 0, len(files))
	for _, path := range files {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			continue
		}
		skip := false
		for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
			if strings.HasPrefix(part, ".") || gemSkipDirs[part] {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, path)
		}
	}
	return out
}

func appendUnique[T comparable](values []T, value T) []T {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// addFleetLabel records that label has name at version — the shared
// name→version→labels shape used by the gem, npm and node fleet collectors.
func addFleetLabel(versions map[string]map[string][]string, name, version, label string) {
	if versions[name] == nil {
		versions[name] = map[string][]string{}
	}
	versions[name][version] = appendUnique(versions[name][version], label)
}

// sortFleetVersions sorts each version's label list for deterministic output.
func sortFleetVersions(versions map[string]map[string][]string) {
	for _, byVersion := range versions {
		for version := range byVersion {
			slices.Sort(byVersion[version])
		}
	}
}

// sortVersionStrings orders version strings by the parsed comparison when both
// sides parse, falling back to lexical order. Shared by the gem, npm and node
// version-list builders.
func sortVersionStrings[V any](names []string, parse func(string) (V, bool), compare func(a, b V) int) {
	slices.SortFunc(names, func(a, b string) int {
		left, leftOK := parse(a)
		right, rightOK := parse(b)
		if leftOK && rightOK {
			return compare(left, right)
		}
		return strings.Compare(a, b)
	})
}

// markStaleFields appends the field names whose pointer is present to stale —
// the shared shape behind the GitHub/Forgejo stale-field marking.
func markStaleFields(stale *[]string, fields map[string]bool) {
	for name, present := range fields {
		if present {
			*stale = appendUnique(*stale, name)
		}
	}
	slices.Sort(*stale)
}

func uniqueSorted(values []string) []string {
	out := slices.DeleteFunc(slices.Clone(values), func(value string) bool {
		return value == ""
	})
	slices.Sort(out)
	return slices.Compact(out)
}

func applyGemPolicy(repo *Repository, data gemInventoryData, policy policyFile, old *Repository) {
	repo.Gems = make(map[string]string, len(data.Versions))
	repo.GemEdges = data.Edges
	repo.GemspecDeps = data.GemspecDeps
	repo.LockfileMissing = data.HasGemfile && len(data.Versions) == 0 && !data.HasGemspec
	repo.GemConflicts = []string{}
	for name, versions := range data.Versions {
		versionNames := sortedVersionNames(versions)
		if len(versionNames) > 0 {
			repo.Gems[name] = data.FirstVersion[name]
			if repo.Gems[name] == "" {
				repo.Gems[name] = versionNames[0]
			}
		}
		if len(versionNames) > 1 {
			repo.GemConflicts = append(repo.GemConflicts, gemConflict(name, versionNames, versions))
		}
	}
	slices.Sort(repo.GemConflicts)
	repo.GemChanges = gemChanges(repo.Gems, old)
	applyGemWatch(repo, data, policy)
	applyGemSlotNotes(repo, policy)
	slices.Sort(repo.GemNotes)
}

// gemConflict renders one multi-version conflict as
// "name: v1 (repoA) vs v2 (repoB)".
func gemConflict(name string, versionNames []string, versions map[string][]string) string {
	parts := make([]string, 0, len(versionNames))
	for _, version := range versionNames {
		parts = append(parts, fmt.Sprintf("%s (%s)", version, strings.Join(versions[version], ", ")))
	}
	return name + ": " + strings.Join(parts, " vs ")
}

// gemChanges diffs the repo's gem set against the previous snapshot. A missing
// or gem-less old snapshot yields an empty diff rather than reporting every
// gem as newly added.
func gemChanges(current map[string]string, old *Repository) GemChanges {
	if old == nil || old.Gems == nil {
		return GemChanges{Added: []string{}, Removed: []string{}}
	}
	added, removed := []string{}, []string{}
	for name := range current {
		if _, ok := old.Gems[name]; !ok {
			added = append(added, name)
		}
	}
	for name := range old.Gems {
		if _, ok := current[name]; !ok {
			removed = append(removed, name)
		}
	}
	return GemChanges{Added: uniqueSorted(added), Removed: uniqueSorted(removed)}
}

// applyGemWatch applies policy watch rules: direct dependencies land in
// GemsWatch; watched gems present only transitively produce a note naming
// their pullers.
func applyGemWatch(repo *Repository, data gemInventoryData, policy policyFile) {
	direct := map[string]bool{}
	for _, name := range data.Direct {
		direct[name] = true
	}
	for name := range repo.GemspecDeps {
		direct[name] = true
	}
	repo.GemsWatch = map[string]string{}
	repo.GemNotes = []string{}
	for name, reason := range policy.Gems.Watch {
		if direct[name] {
			repo.GemsWatch[name] = reason
			continue
		}
		if _, present := data.Versions[name]; present {
			repo.GemNotes = append(repo.GemNotes, fmt.Sprintf("watched gem %s (transitive via %s) — %s",
				name, strings.Join(transitivePullers(data.Edges[name]), "/"), reason))
		}
	}
}

// transitivePullers returns the sorted, deduplicated spec names that require
// the gem.
func transitivePullers(edges []GemEdge) []string {
	pullers := []string{}
	for _, edge := range edges {
		pullers = appendUnique(pullers, edge.By)
	}
	slices.Sort(pullers)
	return pullers
}

// applyGemSlotNotes notes when an acceptable alternative occupies a policy
// slot that prefers something else.
func applyGemSlotNotes(repo *Repository, policy policyFile) {
	slots := make([]string, 0, len(policy.Gems.Slots))
	for slot := range policy.Gems.Slots {
		slots = append(slots, slot)
	}
	slices.Sort(slots)
	for _, slot := range slots {
		rule := policy.Gems.Slots[slot]
		for _, alternative := range rule.OK {
			if _, present := repo.Gems[alternative]; present {
				repo.GemNotes = append(repo.GemNotes, fmt.Sprintf("%s: %s (prefer %s)", slot, alternative, rule.Prefer))
			}
		}
	}
}
