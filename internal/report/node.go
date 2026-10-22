package report

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type packageManifest struct {
	Name             string            `json:"name"`
	Version          string            `json:"version"`
	PackageManager   string            `json:"packageManager"`
	Dependencies     map[string]string `json:"dependencies"`
	DevDependencies  map[string]string `json:"devDependencies"`
	OptionalDeps     map[string]string `json:"optionalDependencies"`
	PeerDependencies map[string]string `json:"peerDependencies"`
	Engines          struct {
		Node string `json:"node"`
	} `json:"engines"`
}

type npmLockfile struct {
	Name            string                    `json:"name"`
	Version         string                    `json:"version"`
	LockfileVersion int                       `json:"lockfileVersion"`
	Packages        map[string]npmLockPackage `json:"packages"`
	Dependencies    map[string]npmLockNode    `json:"dependencies"`
}

type npmLockPackage struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	Link                 bool              `json:"link"`
	Dependencies         map[string]string `json:"dependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
}

type npmLockNode struct {
	Version      string                 `json:"version"`
	Requires     map[string]string      `json:"requires"`
	Dependencies map[string]npmLockNode `json:"dependencies"`
}

func collectNPMProjects(root string, policy policyFile) ([]NPMProject, []string) {
	paths := matchingNodeFiles(root, "package.json")
	projects := make([]NPMProject, 0, len(paths))
	issues := []string{}
	for _, manifestPath := range paths {
		manifest, err := readPackageManifest(manifestPath)
		relDir, relErr := filepath.Rel(root, filepath.Dir(manifestPath))
		if relErr != nil {
			relDir = filepath.Dir(manifestPath)
		}
		if relDir == "." {
			relDir = ""
		}
		relDir = filepath.ToSlash(relDir)
		if err != nil {
			issues = append(issues, fmt.Sprintf("%s/package.json: invalid JSON", relDir))
			continue
		}
		project, projectIssues := collectNPMProject(root, filepath.Dir(manifestPath), relDir, manifest, policy)
		projects = append(projects, project)
		issues = append(issues, projectIssues...)
	}
	slices.Sort(issues)
	return projects, issues
}

func readPackageManifest(path string) (packageManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return packageManifest{}, err
	}
	var manifest packageManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return packageManifest{}, err
	}
	return manifest, nil
}

func collectNPMProject(root, dir, relative string, manifest packageManifest, policy policyFile) (NPMProject, []string) {
	project := NPMProject{
		Path:             relative,
		Name:             manifest.Name,
		Version:          manifest.Version,
		PackageManager:   manifest.PackageManager,
		NodeEngine:       manifest.Engines.Node,
		Direct:           map[string]string{},
		Packages:         map[string]string{},
		PackageVersions:  map[string][]string{},
		MultipleVersions: []string{},
		Edges:            map[string][]NPMEdge{},
		Watched:          map[string]string{},
	}
	if project.Name == "" {
		project.Name = relative
		if project.Name == "" {
			project.Name = "."
		}
	}
	mergeNPMDirect(project.Direct, manifest.Dependencies)
	mergeNPMDirect(project.Direct, manifest.DevDependencies)
	mergeNPMDirect(project.Direct, manifest.OptionalDeps)
	mergeNPMDirect(project.Direct, manifest.PeerDependencies)

	project.NodeVersion, project.NodeVersionFile = nearestNodeVersion(root, dir)
	issues := []string{}
	if issue := loadNPMLockfile(root, dir, &project); issue != "" {
		issues = append(issues, issue)
	}
	if project.LockfileMissing {
		issues = append(issues, fmt.Sprintf("%s: package.json without lockfile", project.Path))
	}
	issues = append(issues, nodeVersionIssues(project, policy)...)
	for packageName := range project.Direct {
		if reason, watched := policy.NPM.Watch[packageName]; watched {
			project.Watched[packageName] = reason
		}
	}
	return project, issues
}

// loadNPMLockfile parses the project's lockfile into project fields, flagging
// LockfileMissing when no lockfile exists and the manifest has dependencies.
// Returns a report issue on parse failure.
func loadNPMLockfile(root, dir string, project *NPMProject) string {
	lockPath := npmLockfilePath(dir)
	if lockPath == "" {
		project.LockfileMissing = len(project.Direct) > 0
		return ""
	}
	project.Lockfile = repoRelativePath(root, lockPath)
	if err := collectNPMLock(lockPath, project); err != nil {
		return fmt.Sprintf("%s: invalid npm lockfile", project.Lockfile)
	}
	return ""
}

// nodeVersionIssues reports policy violations around the project's pinned
// Node version and engines.node range.
func nodeVersionIssues(project NPMProject, policy policyFile) []string {
	issues := []string{}
	if policy.Checks.RequireNodeVersionFile && project.NodeVersion == "" {
		issues = append(issues, fmt.Sprintf("%s: no .nvmrc or .node-version file", project.Path))
	}
	if project.NodeVersion != "" && project.NodeEngine != "" &&
		!nodePinSatisfiesRange(project.NodeEngine, project.NodeVersion) {
		issues = append(issues, fmt.Sprintf("%s: node %s does not satisfy engines.node %s",
			project.Path, project.NodeVersion, project.NodeEngine))
	}
	if wanted := strings.TrimSpace(policy.Versions.Node); wanted != "" {
		if project.NodeVersion != "" && !nodePinMatches(project.NodeVersion, wanted) {
			issues = append(issues, fmt.Sprintf("%s: Node pin %s does not satisfy %s", project.Path, project.NodeVersion, wanted))
		}
		if project.NodeEngine != "" && !nodePinSatisfiesRange(project.NodeEngine, wanted) {
			issues = append(issues, fmt.Sprintf("%s: engines.node %s excludes policy %s", project.Path, project.NodeEngine, wanted))
		}
	}
	return issues
}

func mergeNPMDirect(target, source map[string]string) {
	for name, requirement := range source {
		target[name] = requirement
	}
}

func npmLockfilePath(dir string) string {
	for _, name := range []string{"npm-shrinkwrap.json", "package-lock.json"} {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path
		}
	}
	return ""
}

func collectNPMLock(path string, project *NPMProject) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var lock npmLockfile
	if err := json.Unmarshal(data, &lock); err != nil {
		return err
	}
	project.LockfileVersion = lock.LockfileVersion
	versions := map[string]map[string]bool{}
	if len(lock.Packages) > 0 {
		collectLockPackages(lock.Packages, project, versions)
	} else {
		lockNodeDependencies(lock.Dependencies, project.Name, project.Path, project, versions)
	}
	for dependency, requirement := range project.Direct {
		project.Edges[dependency] = appendUnique(project.Edges[dependency], NPMEdge{
			By: project.Name, Req: requirement, Project: project.Path,
		})
	}
	recordPackageVersions(project, versions)
	return nil
}

// collectLockPackages walks the v2/v3 'packages' lockfile map in key order,
// recording installable versions and dependency edges per package.
func collectLockPackages(packages map[string]npmLockPackage, project *NPMProject, versions map[string]map[string]bool) {
	for _, key := range slices.Sorted(maps.Keys(packages)) {
		entry := packages[key]
		if key == "" || entry.Link || entry.Version == "" {
			continue
		}
		name := entry.Name
		if name == "" {
			name = npmNameFromLockPath(key)
		}
		if name == "" {
			continue
		}
		addNPMVersion(versions, name, entry.Version)
		addLockPackageEdges(project, name, entry)
	}
}

// addLockPackageEdges records dependency edges from one locked package across
// its runtime, optional and peer dependency groups.
func addLockPackageEdges(project *NPMProject, name string, entry npmLockPackage) {
	groups := []map[string]string{entry.Dependencies, entry.OptionalDependencies, entry.PeerDependencies}
	for _, group := range groups {
		for _, dependency := range slices.Sorted(maps.Keys(group)) {
			project.Edges[dependency] = appendUnique(project.Edges[dependency], NPMEdge{
				By: name, Req: group[dependency], Project: project.Path,
			})
		}
	}
}

// recordPackageVersions fills Packages/PackageVersions with sorted version
// lists and MultipleVersions with the conflicts.
func recordPackageVersions(project *NPMProject, versions map[string]map[string]bool) {
	for name, byVersion := range versions {
		versionNames := slices.Collect(maps.Keys(byVersion))
		sortVersionStrings(versionNames, parseNPMVersion, compareNPMVersions)
		if len(versionNames) > 0 {
			project.Packages[name] = versionNames[len(versionNames)-1]
			project.PackageVersions[name] = versionNames
		}
		if len(versionNames) > 1 {
			project.MultipleVersions = append(project.MultipleVersions,
				fmt.Sprintf("%s: %s", name, strings.Join(versionNames, " vs ")))
		}
	}
	slices.Sort(project.MultipleVersions)
}

func matchingNodeFiles(root, filename string) []string {
	patterns := []string{
		filepath.Join(root, filename),
		filepath.Join(root, "*", filename),
		filepath.Join(root, "*", "*", filename),
	}
	var files []string
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		for _, path := range matches {
			relative, err := filepath.Rel(root, path)
			if err != nil || skipNodePath(relative) {
				continue
			}
			files = append(files, path)
		}
	}
	return uniqueSorted(files)
}

func skipNodePath(relative string) bool {
	for _, part := range strings.Split(filepath.ToSlash(relative), "/") {
		if strings.HasPrefix(part, ".") || gemSkipDirs[part] {
			return true
		}
	}
	return false
}

func nearestNodeVersion(root, dir string) (version, relativeFile string) {
	root, _ = filepath.Abs(root)
	current, _ := filepath.Abs(dir)
	for {
		for _, filename := range []string{".nvmrc", ".node-version", ".tool-versions"} {
			path := filepath.Join(current, filename)
			if data, err := os.ReadFile(path); err == nil {
				value := nodeVersionInFile(filename, string(data))
				if value != "" {
					relative, relErr := filepath.Rel(root, path)
					if relErr != nil {
						relative = path
					}
					return normalizeNodePin(value), filepath.ToSlash(relative)
				}
			}
		}
		if current == root {
			break
		}
		parent := filepath.Dir(current)
		if parent == current || !pathWithin(root, parent) {
			break
		}
		current = parent
	}
	return "", ""
}

// nodeVersionInFile extracts the node pin from a version file: .nvmrc and
// .node-version carry the value on the first line; .tool-versions carries it
// on a "nodejs <version>" line.
func nodeVersionInFile(filename, content string) string {
	if filename == ".tool-versions" {
		for _, line := range strings.Split(content, "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "nodejs" {
				return fields[1]
			}
		}
		return ""
	}
	return strings.TrimSpace(content)
}

func normalizeNodePin(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "node-")
	value = strings.TrimPrefix(value, "v")
	return value
}

func nodePinMatches(pin, policy string) bool {
	return nodePinSatisfiesRange(policy, normalizeNodePin(pin))
}

func nodePinSatisfiesRange(expression, pin string) bool {
	if _, ok := parseNPMVersion(pin); !ok {
		if _, ok := parseNPMPartial(pin); !ok {
			return true
		}
	}
	return npmRangeSatisfies(expression, pin)
}

func npmNameFromLockPath(path string) string {
	index := strings.LastIndex(path, "node_modules/")
	if index < 0 {
		return ""
	}
	namePath := path[index+len("node_modules/"):]
	parts := strings.Split(namePath, "/")
	if len(parts) == 0 || parts[0] == "" {
		return ""
	}
	if strings.HasPrefix(parts[0], "@") && len(parts) > 1 {
		return parts[0] + "/" + parts[1]
	}
	return parts[0]
}

func addNPMVersion(versions map[string]map[string]bool, name, version string) {
	if _, ok := parseNPMVersion(version); !ok {
		return
	}
	if versions[name] == nil {
		versions[name] = map[string]bool{}
	}
	versions[name][version] = true
}

func lockNodeDependencies(
	dependencies map[string]npmLockNode,
	parent, projectPath string,
	project *NPMProject,
	versions map[string]map[string]bool,
) {
	names := make([]string, 0, len(dependencies))
	for name := range dependencies {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		entry := dependencies[name]
		addNPMVersion(versions, name, entry.Version)
		required := slices.Sorted(maps.Keys(entry.Requires))
		for _, dependency := range required {
			requirement := entry.Requires[dependency]
			project.Edges[dependency] = appendUnique(project.Edges[dependency], NPMEdge{
				By: parent, Req: requirement, Project: projectPath,
			})
		}
		lockNodeDependencies(entry.Dependencies, name, projectPath, project, versions)
	}
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
