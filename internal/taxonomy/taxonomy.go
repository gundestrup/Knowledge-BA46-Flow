// Package taxonomy loads the optional, human-owned tag allowlist for a shelf.
package taxonomy

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
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

// ReadFromRoot reads taxonomy.yaml inside the repository root. The bool
// distinguishes an absent file from a present, empty file. Symlinks,
// non-regular files and oversized files are rejected.
func ReadFromRoot(root *os.Root) ([]byte, bool, error) {
	info, err := root.Lstat(fileName)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("inspect %s: %w", fileName, err)
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s must be a regular file", fileName)
	}
	file, err := root.Open(fileName)
	if err != nil {
		return nil, false, fmt.Errorf("open %s: %w", fileName, err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, false, fmt.Errorf("read %s: %w", fileName, readErr)
	}
	if closeErr != nil {
		return nil, false, fmt.Errorf("close %s: %w", fileName, closeErr)
	}
	if len(data) > maxBytes {
		return nil, false, fmt.Errorf("%s exceeds %d bytes", fileName, maxBytes)
	}
	return data, true, nil
}

// LoadFromRoot reads and parses taxonomy.yaml. Absence means the shelf has no
// taxonomy policy; malformed files fail closed.
func LoadFromRoot(root *os.Root) (*Vocabulary, error) {
	data, exists, err := ReadFromRoot(root)
	if err != nil || !exists {
		return nil, err
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

type tagRule struct {
	Tag              string `json:"tag"`
	Allowed          bool   `json:"allowed"`
	Kind             bool   `json:"kind"`
	RequiresLanguage bool   `json:"requires_language"`
	LanguageSubject  bool   `json:"language_subject"`
}

// ScopeDigest hashes the complete taxonomy projection used for one document:
// its prompt tags, current-tag validity, and the validation rules for every
// tag it may preserve or propose. Proposal validation is restricted to this
// same scope, so unrelated vocabulary changes do not affect the result.
func (v *Vocabulary) ScopeDigest(relativePath string, currentTags []string) string {
	if v == nil {
		return ""
	}
	promptTags := v.PromptTags(relativePath, currentTags)
	ruleTags := slices.Clone(promptTags)
	ruleTags = append(ruleTags, currentTags...)
	ruleTags = unique(ruleTags)
	sort.Strings(ruleTags)
	rules := make([]tagRule, 0, len(ruleTags))
	for _, tag := range ruleTags {
		_, kind := v.kinds[tag]
		_, requiresLanguage := v.requiresLanguage[tag]
		_, languageSubject := v.languageSubjects[tag]
		rules = append(rules, tagRule{
			Tag:              tag,
			Allowed:          v.Allows(tag),
			Kind:             kind,
			RequiresLanguage: requiresLanguage,
			LanguageSubject:  languageSubject,
		})
	}
	var projection strings.Builder
	writeStrings := func(name string, values []string) {
		projection.WriteString(name)
		projection.WriteByte('=')
		projection.WriteString(strconv.Itoa(len(values)))
		projection.WriteByte(';')
		for _, value := range values {
			projection.WriteString(strconv.Itoa(len(value)))
			projection.WriteByte(':')
			projection.WriteString(value)
		}
	}
	writeStrings("prompt", promptTags)
	writeStrings("current-issues", v.InvalidTagSet(currentTags))
	projection.WriteString("rules=")
	projection.WriteString(strconv.Itoa(len(rules)))
	projection.WriteByte(';')
	for _, rule := range rules {
		projection.WriteString(strconv.Itoa(len(rule.Tag)))
		projection.WriteByte(':')
		projection.WriteString(rule.Tag)
		for _, flag := range []bool{rule.Allowed, rule.Kind, rule.RequiresLanguage, rule.LanguageSubject} {
			if flag {
				projection.WriteByte('1')
			} else {
				projection.WriteByte('0')
			}
		}
	}
	sum := sha256.Sum256([]byte(projection.String()))
	return hex.EncodeToString(sum[:])
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

// ValidateTagSetChange permits existing human-authored values to remain, but
// only permits new values offered in this document's prompt scope. It applies
// the same kind and language constraints used by InvalidTagSet.
func (v *Vocabulary) ValidateTagSetChange(relativePath string, original []string, value any) error {
	if v == nil {
		return nil
	}
	proposed, ok := stringList(value)
	if !ok {
		return fmt.Errorf("tags must be a string or list of strings")
	}
	remaining := make(map[string]int, len(original))
	for _, tag := range original {
		remaining[tag]++
	}
	scope := make(map[string]struct{})
	for _, tag := range v.PromptTags(relativePath, original) {
		scope[tag] = struct{}{}
	}
	for _, tag := range proposed {
		if remaining[tag] > 0 {
			remaining[tag]--
			continue
		}
		if !v.Allows(tag) {
			return fmt.Errorf("tag %q is not listed in taxonomy.yaml", tag)
		}
		if _, ok := scope[tag]; !ok {
			return fmt.Errorf("tag %q is not in the taxonomy prompt scope for %s", tag, relativePath)
		}
	}
	if !slices.Equal(original, proposed) {
		invalid := append(v.kindViolations(proposed), v.requiresLanguageViolations(proposed)...)
		if len(invalid) > 0 {
			return fmt.Errorf("invalid taxonomy tag set: %s", strings.Join(invalid, ", "))
		}
	}
	return nil
}

// ValidateTagChanges validates a complete proposed document using the
// document-relative taxonomy scope. Existing human-owned tags may remain.
func (v *Vocabulary) ValidateTagChanges(relativePath, original, proposed string) error {
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
	return v.ValidateTagSetChange(
		relativePath,
		frontmatter.StringList(oldFields, "tags"),
		frontmatter.StringList(newFields, "tags"),
	)
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
