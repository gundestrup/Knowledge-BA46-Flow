// Package taxonomy loads the optional, human-owned tag allowlist for a shelf.
package taxonomy

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"knowledge-ba46-flow/internal/frontmatter"
)

const (
	fileName = "taxonomy.yaml"
	maxBytes = 1 << 20
)

var tagPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

var kindDirectories = map[string]string{
	"cms":             "cms",
	"databases":       "database",
	"frameworks":      "framework",
	"languages":       "programming-language",
	"lessons-learned": "lesson",
	"libraries":       "library",
	"os":              "operating-system",
	"patterns":        "pattern",
	"prompts":         "prompt",
	"repos":           "repo",
	"tools":           "tool",
	"workflows":       "workflow",
}

type sourceFile struct {
	Version          int                 `yaml:"version"`
	Kind             []string            `yaml:"kind"`
	KindMajor        []string            `yaml:"kind-major"`
	KindDependent    []string            `yaml:"kind-dependent"`
	RequiresLanguage []string            `yaml:"requires-language"`
	Subject          map[string][]string `yaml:"subject"`
	Function         []string            `yaml:"function"`
}

type Vocabulary struct {
	allowed          map[string]struct{}
	kinds            map[string]struct{}
	subjects         map[string]struct{}
	languageSubjects map[string]struct{}
	functions        map[string]struct{}
	requiresLanguage map[string]struct{}
}

// LoadFromRoot reads taxonomy.yaml inside the repository root. Absence means
// the shelf has no taxonomy policy; malformed files fail closed.
func LoadFromRoot(root *os.Root) (*Vocabulary, error) {
	info, err := root.Lstat(fileName)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", fileName, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", fileName)
	}
	file, err := root.Open(fileName)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", fileName, err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read %s: %w", fileName, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close %s: %w", fileName, closeErr)
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("%s exceeds %d bytes", fileName, maxBytes)
	}
	return parse(data)
}

func Load(repositoryPath string) (*Vocabulary, error) {
	root, err := os.OpenRoot(repositoryPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return LoadFromRoot(root)
}

func parse(data []byte) (*Vocabulary, error) {
	var source sourceFile
	if err := yaml.Unmarshal(data, &source); err != nil {
		return nil, fmt.Errorf("parse %s: %w", fileName, err)
	}
	if source.Version != 1 || len(source.Kind) == 0 || len(source.Subject) == 0 {
		return nil, fmt.Errorf("unsupported or incomplete %s", fileName)
	}
	v := &Vocabulary{
		allowed:          make(map[string]struct{}),
		kinds:            make(map[string]struct{}),
		subjects:         make(map[string]struct{}),
		languageSubjects: make(map[string]struct{}),
		functions:        make(map[string]struct{}),
		requiresLanguage: make(map[string]struct{}),
	}
	if err := addValues(v.allowed, source.Kind); err != nil {
		return nil, fmt.Errorf("%s kind: %w", fileName, err)
	}
	for _, kind := range source.Kind {
		v.kinds[kind] = struct{}{}
	}
	for group, values := range source.Subject {
		if err := addValues(v.allowed, values); err != nil {
			return nil, fmt.Errorf("%s subject %q: %w", fileName, group, err)
		}
		for _, value := range values {
			v.subjects[value] = struct{}{}
			if group == "languages" {
				v.languageSubjects[value] = struct{}{}
			}
		}
	}
	if err := addValues(v.allowed, source.Function); err != nil {
		return nil, fmt.Errorf("%s function: %w", fileName, err)
	}
	if err := addValues(v.allowed, source.KindMajor); err != nil {
		return nil, fmt.Errorf("%s kind-major: %w", fileName, err)
	}
	if err := addValues(v.allowed, source.KindDependent); err != nil {
		return nil, fmt.Errorf("%s kind-dependent: %w", fileName, err)
	}
	for _, kinds := range [][]string{source.KindMajor, source.KindDependent} {
		for _, kind := range kinds {
			if !contains(source.Kind, kind) {
				return nil, fmt.Errorf("%s kind group contains undeclared kind %q", fileName, kind)
			}
		}
	}
	if err := addValues(v.allowed, source.RequiresLanguage); err != nil {
		return nil, fmt.Errorf("%s requires-language: %w", fileName, err)
	}
	for _, kind := range source.RequiresLanguage {
		v.requiresLanguage[kind] = struct{}{}
		if !contains(source.Kind, kind) || !contains(source.KindDependent, kind) {
			return nil, fmt.Errorf("%s requires-language kind %q must be declared in kind and kind-dependent", fileName, kind)
		}
	}
	for _, function := range source.Function {
		v.functions[function] = struct{}{}
	}
	return v, nil
}

// Parse validates taxonomy YAML bytes without filesystem access.
func Parse(data []byte) (*Vocabulary, error) {
	return parse(data)
}

func addValues(dst map[string]struct{}, values []string) error {
	for _, value := range values {
		if value != strings.TrimSpace(value) || !tagPattern.MatchString(value) {
			return fmt.Errorf("values must be lowercase tag slugs")
		}
		dst[value] = struct{}{}
	}
	return nil
}

func (v *Vocabulary) Allows(tag string) bool {
	if v == nil {
		return false
	}
	_, ok := v.allowed[tag]
	return ok
}

func (v *Vocabulary) AllowedTags() []string {
	if v == nil {
		return nil
	}
	tags := make([]string, 0, len(v.allowed))
	for tag := range v.allowed {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	return tags
}

// PromptTags limits repetitive prompt context to path-derived kinds/subjects,
// current human tags, and the controlled function vocabulary.
func (v *Vocabulary) PromptTags(relativePath string, currentTags []string) []string {
	if v == nil {
		return nil
	}
	tags := v.TagsForPath(relativePath)
	for _, tag := range currentTags {
		if v.Allows(tag) {
			tags = append(tags, tag)
		}
	}
	for function := range v.functions {
		tags = append(tags, function)
	}
	tags = unique(tags)
	sort.Strings(tags)
	return tags
}

// TagsForPath returns only kind/subject tags supported by taxonomy.yaml.
func (v *Vocabulary) TagsForPath(relativePath string) []string {
	if v == nil {
		return nil
	}
	parts := strings.Split(filepath.ToSlash(relativePath), "/")
	if len(parts) < 2 {
		return nil
	}
	if len(parts) == 2 && strings.EqualFold(strings.TrimSuffix(parts[1], filepath.Ext(parts[1])), "readme") {
		return nil
	}
	kind, ok := kindDirectories[parts[0]]
	if !ok || !v.Allows(kind) {
		return nil
	}
	tags := []string{kind}
	for n, part := range parts[1:] {
		if n == len(parts[1:])-1 {
			part = strings.TrimSuffix(part, filepath.Ext(part))
		}
		part = strings.ToLower(strings.TrimSpace(part))
		part = strings.ReplaceAll(strings.ReplaceAll(part, "_", "-"), " ", "-")
		if part == "" || part == "readme" {
			continue
		}
		if _, ok := v.subjects[part]; ok {
			tags = append(tags, part)
		}
	}
	for _, tag := range tags {
		if _, required := v.requiresLanguage[tag]; !required {
			continue
		}
		hasLanguage := false
		for _, subject := range tags {
			if _, ok := v.languageSubjects[subject]; ok {
				hasLanguage = true
				break
			}
		}
		if !hasLanguage {
			return nil
		}
	}
	return unique(tags)
}

func unique(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !slices.Contains(out, value) {
			out = append(out, value)
		}
	}
	return out
}

// UnknownTags reports values not in the taxonomy. Malformed tag shapes are
// left to frontmatter.Validate; this function only checks string values.
func (v *Vocabulary) UnknownTags(value any) []string {
	if v == nil {
		return nil
	}
	tags, ok := stringList(value)
	if !ok {
		return nil
	}
	var unknown []string
	for _, tag := range tags {
		if !v.Allows(tag) && !slices.Contains(unknown, tag) {
			unknown = append(unknown, tag)
		}
	}
	return unknown
}

func (v *Vocabulary) InvalidTagSet(value any) []string {
	if v == nil {
		return nil
	}
	tags, ok := stringList(value)
	if !ok {
		return nil
	}
	invalid := v.UnknownTags(value)
	invalid = append(invalid, v.kindViolations(tags)...)
	return append(invalid, v.requiresLanguageViolations(tags)...)
}

func (v *Vocabulary) kindViolations(tags []string) []string {
	if len(tags) == 0 {
		return []string{"tags require exactly one kind value"}
	}
	kinds := 0
	for _, tag := range tags {
		if _, ok := v.kinds[tag]; ok {
			kinds++
		}
	}
	switch {
	case kinds == 0:
		return []string{"tags require exactly one kind value"}
	case kinds > 1:
		return []string{"tags must contain only one kind value"}
	default:
		return nil
	}
}

// InvalidTagsForPath applies InvalidTagSet with path-aware exemptions: an
// index README.md aggregates many subjects, so it is exempt from the
// requires-language rule. All other checks are identical.
func (v *Vocabulary) InvalidTagsForPath(relativePath string, value any) []string {
	if v == nil {
		return nil
	}
	tags, ok := stringList(value)
	if !ok {
		return nil
	}
	invalid := v.UnknownTags(value)
	invalid = append(invalid, v.kindViolations(tags)...)
	if filepath.Base(relativePath) != "README.md" {
		invalid = append(invalid, v.requiresLanguageViolations(tags)...)
	}
	return invalid
}

func (v *Vocabulary) requiresLanguageViolations(tags []string) []string {
	requiresLanguage := false
	hasLanguage := false
	for _, tag := range tags {
		if _, ok := v.requiresLanguage[tag]; ok {
			requiresLanguage = true
		}
		if _, ok := v.languageSubjects[tag]; ok {
			hasLanguage = true
		}
	}
	if requiresLanguage && !hasLanguage {
		return []string{"framework/library/cms tags require a subject.languages tag"}
	}
	return nil
}

// ValidateTagChanges permits existing human-authored values to remain, but
// rejects new tag values not approved by the current taxonomy.
func (v *Vocabulary) ValidateTagChanges(original, proposed string) error {
	if v == nil {
		return nil
	}
	oldFields, _, err := frontmatter.Parse(original)
	if err != nil {
		return err
	}
	newFields, _, err := frontmatter.Parse(proposed)
	if err != nil {
		return err
	}
	oldTags := frontmatter.StringList(oldFields, "tags")
	remaining := make(map[string]int, len(oldTags))
	for _, tag := range oldTags {
		remaining[tag]++
	}
	newTags := frontmatter.StringList(newFields, "tags")
	for _, tag := range newTags {
		if remaining[tag] > 0 {
			remaining[tag]--
			continue
		}
		if !v.Allows(tag) {
			return fmt.Errorf("tag %q is not listed in taxonomy.yaml", tag)
		}
	}
	if !slices.Equal(oldTags, newTags) {
		invalid := append(v.kindViolations(newTags), v.requiresLanguageViolations(newTags)...)
		if len(invalid) > 0 {
			return fmt.Errorf("invalid taxonomy tag set: %s", strings.Join(invalid, ", "))
		}
	}
	return nil
}

func stringList(value any) ([]string, bool) {
	switch typed := value.(type) {
	case string:
		return []string{typed}, true
	case []string:
		return slices.Clone(typed), true
	case []any:
		var tags []string
		for _, item := range typed {
			tag, ok := item.(string)
			if !ok {
				return nil, false
			}
			tags = append(tags, tag)
		}
		return tags, true
	default:
		return nil, false
	}
}

func contains(values []string, want string) bool {
	return slices.Contains(values, want)
}
