package report

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// toolchainDirs are skipped while looking for version pins — vendored,
// generated, or VCS trees never describe the repository's own toolchain.
var toolchainSkipDirs = map[string]bool{
	".git": true, "vendor": true, "node_modules": true, ".ruby-lsp": true,
}

// toolVersionsAliases maps .tool-versions plugin names that differ from the
// policy product name. Every other plugin name is recorded verbatim — custom
// policy keys match by plugin name.
var toolVersionsAliases = map[string]string{
	"nodejs": "node", "golang": "go",
}

// dedicatedVersionFiles are checked by their own routines (rubyVersionIssues,
// nearestNodeVersion) — the generic .<name>-version rule must not double them.
var dedicatedVersionFiles = map[string]bool{
	".ruby-version": true, ".nvmrc": true, ".node-version": true,
}

// genericVersionFile matches .<name>-version conventions (pyenv, goenv,
// phpenv, tfenv…): the captured name becomes the product.
var genericVersionFile = regexp.MustCompile(`^\.([a-zA-Z][\w]*)-version$`)

// rubygemsWorkflowPin matches the rubygems: input ruby/setup-ruby accepts in
// CI workflows — the only common place a RubyGems version is pinned.
var rubygemsWorkflowPin = regexp.MustCompile(`(?m)^\s*rubygems:\s*['"]?([\w.\-]+)`)

// ToolchainPin is one detected version pin inside a repository: which product,
// which file stated it, and the pinned value. Constraint marks declared
// *ranges* (composer require.php) — the check direction is reversed: the
// policy value must satisfy the repository's constraint.
type ToolchainPin struct {
	Product    string `json:"product"`
	File       string `json:"file"`
	Version    string `json:"version"`
	Constraint bool   `json:"-"`
}

// toolchainIssues evaluates detected pins against the effective policy
// values. Only products with a non-empty policy are checked; repositories
// without a matching pin file produce no issue (required-file enforcement is
// the per-language require_* checks).
func toolchainIssues(dir string, policy policyFile) ([]ToolchainPin, []string) {
	pins := collectToolchainPins(dir)
	issues := []string{}
	// .ruby-version stays under rubyVersionIssues and node pins under
	// nearestNodeVersion (which also reads .tool-versions) — the same pin
	// must not produce two differently worded findings. Everything else is
	// driven by the product table plus versions.custom.
	wanted := policyWanted(policy)
	for _, pin := range pins {
		expression := wanted[pin.Product]
		if expression == "" || isPolicyChannel(expression) {
			continue
		}
		if pin.Constraint {
			// The pin is a declared range (composer require.php): flag when
			// the policy value falls outside it, like the engines.node check.
			if !npmRangeSatisfies(pin.Version, expression) {
				issues = append(issues, fmt.Sprintf("%s: %s %s excludes policy %s",
					pin.File, pin.Product, pin.Version, expression))
			}
			continue
		}
		if !npmRangeSatisfies(expression, normalizeToolchainPin(pin.Version)) {
			issues = append(issues, fmt.Sprintf("%s: %s %s does not satisfy %s",
				pin.File, pin.Product, pin.Version, expression))
		}
	}
	return pins, issues
}

// normalizeToolchainPin strips the decorations version files put around
// numbers (a leading v or go prefix, a python-build suffix is left for the
// lenient range parser to pass on).
func normalizeToolchainPin(version string) string {
	version = strings.TrimSpace(version)
	version = strings.TrimPrefix(version, "go")
	version = strings.TrimPrefix(version, "v")
	return version
}

// collectToolchainPins walks the repository once and records every version
// pin file it understands: .python-version, .go-version, .tool-versions,
// go.mod (go and toolchain directives), Gemfile.lock BUNDLED WITH, and
// rubygems: pins in CI workflows.
func collectToolchainPins(dir string) []ToolchainPin {
	var pins []ToolchainPin
	_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			if path != dir && toolchainSkipDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		pins = append(pins, toolchainFilePins(path, rel, entry.Name())...)
		return nil
	})
	slices.SortFunc(pins, func(a, b ToolchainPin) int {
		if cmp := strings.Compare(a.Product, b.Product); cmp != 0 {
			return cmp
		}
		return strings.Compare(a.File, b.File)
	})
	return pins
}

// toolchainFilePins extracts the pins a single file may carry based on its
// name and location.
func toolchainFilePins(path, rel, name string) []ToolchainPin {
	switch name {
	case ".tool-versions":
		return toolVersionsFilePins(path, rel)
	case "go.mod":
		return goModPins(path, rel)
	case "Gemfile.lock":
		return bundledWithPins(path, rel)
	case "composer.json":
		return composerPins(path, rel)
	}
	if rubygemsWorkflowName(rel, name) {
		return rubygemsWorkflowPins(path, rel)
	}
	if match := genericVersionFile.FindStringSubmatch(name); match != nil &&
		!dedicatedVersionFiles[name] {
		return pinnedFilePins(path, rel, normalizeProductName(match[1]))
	}
	return nil
}

// pinnedFilePins reads a one-version-per-file pin (pyenv/goenv style).
func pinnedFilePins(path, rel, product string) []ToolchainPin {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	version := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	if version == "" {
		return nil
	}
	return []ToolchainPin{{Product: product, File: rel, Version: version}}
}

// toolVersionsFilePins parses asdf-style "name version..." lines. Plugin
// names map through the alias table; anything else is recorded under its own
// name so versions.custom products resolve against it.
func toolVersionsFilePins(path, rel string) []ToolchainPin {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pins []ToolchainPin
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		product := normalizeProductName(fields[0])
		if alias, ok := toolVersionsAliases[product]; ok {
			product = alias
		}
		pins = append(pins, ToolchainPin{Product: product, File: rel, Version: fields[1]})
	}
	return pins
}

// goModPins reads the go directive and, when present, the more specific
// toolchain directive (toolchain goX.Y.Z is what actually pins builds).
func goModPins(path, rel string) []ToolchainPin {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pins []ToolchainPin
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		switch {
		case len(fields) >= 2 && fields[0] == "go":
			pins = append(pins, ToolchainPin{Product: "go", File: rel, Version: fields[1]})
		case len(fields) >= 2 && fields[0] == "toolchain":
			version := strings.TrimPrefix(fields[1], "go")
			pins = append(pins, ToolchainPin{Product: "go", File: rel + " (toolchain)", Version: version})
		}
	}
	return pins
}

// composerPins reads PHP version declarations: require.php is a declared
// *range* (a constraint — the policy value must satisfy it, like
// engines.node), while config.platform.php pins a concrete version.
func composerPins(path, rel string) []ToolchainPin {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var composer struct {
		Require map[string]string `json:"require"`
		Config  struct {
			Platform map[string]string `json:"platform"`
		} `json:"config"`
	}
	if err := json.Unmarshal(data, &composer); err != nil {
		return nil
	}
	var pins []ToolchainPin
	if constraint := strings.TrimSpace(composer.Require["php"]); constraint != "" {
		pins = append(pins, ToolchainPin{Product: "php", File: rel, Version: constraint, Constraint: true})
	}
	if platform := strings.TrimSpace(composer.Config.Platform["php"]); platform != "" {
		pins = append(pins, ToolchainPin{Product: "php", File: rel + " (platform)", Version: platform})
	}
	return pins
}

// bundledWithPins reads the trailing BUNDLED WITH section of a Gemfile.lock.
func bundledWithPins(path, rel string) []ToolchainPin {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "BUNDLED WITH" || i+1 >= len(lines) {
			continue
		}
		if version := strings.TrimSpace(lines[i+1]); version != "" {
			return []ToolchainPin{{Product: "bundler", File: rel, Version: version}}
		}
	}
	return nil
}

func rubygemsWorkflowName(rel, name string) bool {
	if !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
		return false
	}
	return strings.HasPrefix(rel, ".github/workflows/") ||
		strings.HasPrefix(rel, ".forgejo/workflows/")
}

func rubygemsWorkflowPins(path, rel string) []ToolchainPin {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pins []ToolchainPin
	for _, match := range rubygemsWorkflowPin.FindAllSubmatch(data, -1) {
		pins = append(pins, ToolchainPin{
			Product: "rubygems", File: rel, Version: string(match[1]),
		})
	}
	return pins
}
