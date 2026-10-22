package report

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var (
	procfileRunner = regexp.MustCompile(`foreman|overmind|hivemind|Procfile`)
	semgrepCloud   = regexp.MustCompile(`semgrep\s+(ci|login)\b`)
	semgrepPack    = regexp.MustCompile(`--config[=\s]+["']?[pr]/`)
	semgrepYAMLRef = regexp.MustCompile(`(?m)^\s*-\s*[pr]/|extends:.*[pr]/`)
)

func rubyVersionIssues(dir, wanted string, tags map[string][]string, requireFile bool) ([]string, error) {
	var files []string
	skipDirs := map[string]bool{
		".git": true, "vendor": true, ".ruby-lsp": true,
	}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && path != dir && skipDirs[entry.Name()] {
			return filepath.SkipDir
		}
		if !entry.IsDir() && entry.Name() == ".ruby-version" {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(files)
	if len(files) == 0 {
		if slices.Contains(tags["language"], "ruby") && requireFile {
			return []string{"no .ruby-version file"}, nil
		}
		return []string{}, nil
	}

	issues := []string{}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)
		if err != nil {
			issues = append(issues, fmt.Sprintf("%s: unable to read .ruby-version", rel))
			continue
		}
		version := strings.TrimSpace(string(raw))
		if strings.HasPrefix(version, "ruby-") {
			issues = append(issues, fmt.Sprintf("%s: 'ruby-' prefix (%s)", rel, version))
			version = strings.TrimPrefix(version, "ruby-")
		}
		if wanted != "" && !npmRangeSatisfies(wanted, version) {
			issues = append(issues, fmt.Sprintf("%s: %s does not satisfy %s", rel, version, wanted))
		}
	}
	return issues, nil
}

func scannerConfigs(dir string) []string {
	checks := []struct {
		name  string
		path  string
		isDir bool
	}{
		{name: "semgrep", path: filepath.Join(dir, ".semgrep.yml")},
		{name: "sonar", path: filepath.Join(dir, "sonar-project.properties")},
		{name: "lefthook", path: filepath.Join(dir, "lefthook.yml")},
		{name: "overcommit", path: filepath.Join(dir, ".overcommit.yml")},
		{name: "gh-actions", path: filepath.Join(dir, ".github", "workflows"), isDir: true},
		{name: "forgejo", path: filepath.Join(dir, ".forgejo", "workflows"), isDir: true},
	}
	found := make([]string, 0, len(checks))
	for _, check := range checks {
		info, err := os.Stat(check.path)
		if err == nil && (check.isDir && info.IsDir() || !check.isDir && info.Mode().IsRegular()) {
			found = append(found, check.name)
		}
	}
	return found
}

func semgrepLicense(dir string) string {
	files := []string{
		filepath.Join(dir, "Rakefile"),
		filepath.Join(dir, "Makefile"),
	}
	for _, pattern := range []string{
		filepath.Join(dir, ".forgejo", "workflows", "*.yml"),
		filepath.Join(dir, ".forgejo", "workflows", "*.yaml"),
		filepath.Join(dir, ".github", "workflows", "*.yml"),
		filepath.Join(dir, ".github", "workflows", "*.yaml"),
		filepath.Join(dir, "bin", "*"),
		filepath.Join(dir, "lib", "tasks", "*"),
	} {
		matches, _ := filepath.Glob(pattern)
		files = append(files, matches...)
	}

	var semgrepLines []string
	for _, path := range files {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(content), "\n") {
			if strings.Contains(line, "semgrep") {
				semgrepLines = append(semgrepLines, line)
			}
		}
	}

	semgrepYAML := filepath.Join(dir, ".semgrep.yml")
	hasYAML := false
	if content, err := os.ReadFile(semgrepYAML); err == nil {
		hasYAML = true
		if semgrepYAMLRef.Match(content) {
			return "registry"
		}
	}
	if len(semgrepLines) == 0 && !hasYAML {
		return ""
	}
	for _, line := range semgrepLines {
		if semgrepCloud.MatchString(line) {
			return "cloud-ci"
		}
		if semgrepPack.MatchString(line) {
			return "registry"
		}
	}
	for _, line := range semgrepLines {
		if strings.Contains(line, "--oss-only") {
			return "ce"
		}
	}
	return "local-unmarked"
}

func procfileOrphan(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "Procfile.dev")); err != nil {
		return false
	}
	source, err := os.ReadFile(filepath.Join(dir, "bin", "dev"))
	return err != nil || !procfileRunner.Match(source)
}

func markerStatus(dir string) string {
	_, err := os.Stat(filepath.Join(dir, ".engineering-memory.yml"))
	if err == nil {
		return "present"
	}
	if os.IsNotExist(err) {
		return "missing"
	}
	return "unreadable"
}
