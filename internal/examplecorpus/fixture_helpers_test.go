package examplecorpus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"knowledge-ba46-flow/internal/frontmatter"
)

type expectedSuite struct {
	SchemaVersion            int                     `json:"schema_version"`
	ReferenceDate            string                  `json:"reference_date"`
	Documents                []string                `json:"documents"`
	ExcludedMarkdown         []string                `json:"excluded_markdown"`
	NonMarkdown              []string                `json:"non_markdown"`
	Exclude                  []string                `json:"exclude"`
	ExcludeExtensions        []string                `json:"exclude_extensions"`
	PrivateDocuments         []string                `json:"private_documents"`
	EnrichmentFailures       []string                `json:"enrichment_failures"`
	AnalysisFailures         []string                `json:"analysis_failures"`
	FrontmatterIssueContains map[string][]string     `json:"frontmatter_issue_contains"`
	ExpectedSuggestions      []expectedSuggestion    `json:"expected_suggestions"`
	ExpectedRejections       []expectedRejection     `json:"expected_rejections"`
	PromptContains           map[string][]string     `json:"prompt_contains"`
	DuplicateCandidates      []expectedPair          `json:"duplicate_candidates"`
	ContradictionPairs       []expectedContradiction `json:"contradiction_pairs"`
	NeverDeleteDocuments     []string                `json:"never_delete_documents"`
	ApplyPolicy              string                  `json:"apply_policy"`
}

type expectedSuggestion struct {
	Document          string         `json:"document"`
	Origin            string         `json:"origin"`
	Action            string         `json:"action"`
	TargetPath        string         `json:"target_path"`
	FrontmatterEquals map[string]any `json:"frontmatter_equals"`
	FrontmatterAbsent []string       `json:"frontmatter_absent"`
}

type expectedRejection struct {
	Document string `json:"document"`
	Stage    string `json:"stage"`
	Contains string `json:"contains"`
}

type expectedPair struct {
	Document string `json:"document"`
	Target   string `json:"target"`
}

type expectedContradiction struct {
	Document    string `json:"document"`
	Target      string `json:"target"`
	SourceClaim string `json:"source_claim"`
	TargetClaim string `json:"target_claim"`
}

func projectRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
}

func fixtureRoot(tier string) string {
	return filepath.Join(projectRoot(), "example-knowledge", tier)
}

func readExpected(root string) (expectedSuite, error) {
	var expected expectedSuite
	data, err := os.ReadFile(filepath.Join(root, "expected.json"))
	if err != nil {
		return expected, err
	}
	if err := json.Unmarshal(data, &expected); err != nil {
		return expected, err
	}
	if expected.SchemaVersion != 1 {
		return expected, fmt.Errorf("unsupported fixture manifest schema %d", expected.SchemaVersion)
	}
	return expected, nil
}

func copyFixture(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("fixture contains unexpected symlink %s", relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

func hashTree(root string) (map[string]string, error) {
	hashes := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("fixture contains unexpected symlink %s", path)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		hashes[filepath.ToSlash(relative)] = hex.EncodeToString(sum[:])
		return nil
	})
	return hashes, err
}

func currentPath(prompt string) (string, error) {
	const marker = "Current path: "
	start := strings.Index(prompt, marker)
	if start < 0 {
		return "", errors.New("prompt does not include Current path")
	}
	start += len(marker)
	end := strings.IndexByte(prompt[start:], '\n')
	if end < 0 {
		return "", errors.New("prompt path is not newline-terminated")
	}
	return strings.TrimSpace(prompt[start : start+end]), nil
}

type fakeGenerator struct {
	stage              string
	tier               string
	root               string
	prompts            map[string]string
	enrichmentFailures map[string]bool
	analysisFailures   map[string]bool
}

func newFakeGenerator(stage, tier, root string, expected expectedSuite) *fakeGenerator {
	failures := func(paths []string) map[string]bool {
		set := make(map[string]bool, len(paths))
		for _, path := range paths {
			set[path] = true
		}
		return set
	}
	return &fakeGenerator{
		stage: stage, tier: tier, root: root,
		prompts:            make(map[string]string),
		enrichmentFailures: failures(expected.EnrichmentFailures),
		analysisFailures:   failures(expected.AnalysisFailures),
	}
}

func (g *fakeGenerator) Generate(_ context.Context, prompt string, _ json.RawMessage) (string, error) {
	path, err := currentPath(prompt)
	if err != nil {
		return "", err
	}
	g.prompts[path] = prompt
	if strings.Contains(prompt, "FIXTURE_SECRET_NEVER_SEND_2026") {
		return "", fmt.Errorf("private fixture content reached %s model", g.stage)
	}
	if path == "lessons-learned/private-runbook.md" {
		return "", fmt.Errorf("private fixture path reached %s model", g.stage)
	}
	if g.stage == "enrich" {
		if g.enrichmentFailures[path] {
			return "", errors.New("golden fixture enrichment failure")
		}
		return g.enrichmentReply(path)
	}
	if g.analysisFailures[path] {
		return "not valid JSON", nil
	}
	return g.analysisReply(path, prompt)
}

type enrichmentResponse struct {
	Title   string   `json:"title"`
	Summary string   `json:"summary"`
	Tags    []string `json:"tags"`
}

func (g *fakeGenerator) enrichmentReply(path string) (string, error) {
	content, err := g.readDocument(path)
	if err != nil {
		return "", err
	}
	fields, _, err := frontmatter.Parse(content)
	if err != nil {
		return "", err
	}
	title, _ := fields["title"].(string)
	if title == "" {
		title = firstHeading(content)
	}
	tags := frontmatter.StringList(fields, "tags")
	if len(tags) == 0 {
		tags = []string{"fixture"}
	}
	reply, err := json.Marshal(enrichmentResponse{
		Title: title, Summary: "Deterministic fixture summary for " + path, Tags: tags,
	})
	return string(reply), err
}

type testPatch struct {
	Set    map[string]any `json:"set"`
	Remove []string       `json:"remove"`
}

type testSuggestion struct {
	Action          string  `json:"action"`
	TargetPath      string  `json:"target_path"`
	Reason          string  `json:"reason"`
	Confidence      float64 `json:"confidence"`
	ProposedContent string  `json:"proposed_content"`
}

type analysisResponse struct {
	Title       string           `json:"title"`
	Summary     string           `json:"summary"`
	Tags        []string         `json:"tags"`
	Obsolete    bool             `json:"obsolete"`
	Patch       testPatch        `json:"frontmatter_patch"`
	Suggestions []testSuggestion `json:"suggestions"`
}

func (g *fakeGenerator) analysisReply(path, prompt string) (string, error) {
	content, err := g.readDocument(path)
	if err != nil {
		return "", err
	}
	fields, _, err := frontmatter.Parse(content)
	if err != nil {
		return "", err
	}
	title, _ := fields["title"].(string)
	if title == "" {
		title = firstHeading(content)
	}
	tags := frontmatter.StringList(fields, "tags")
	reply := analysisResponse{
		Title: title, Summary: "Deterministic fixture analysis for " + path,
		Tags: tags, Patch: testPatch{Set: map[string]any{}, Remove: []string{}},
		Suggestions: []testSuggestion{},
	}

	switch {
	case path == "frameworks/deploy/README.md" && g.tier == "basic":
		reply.Patch.Set["related"] = []string{"languages/rollback.md"}
	case path == "frameworks/deploy/README.md" && g.tier != "basic":
		reply.Patch.Set["related"] = []string{"frameworks/deploy/deploy.sh"}
	case path == "patterns/retry-policy.md" && g.tier == "e2e":
		target := "lessons-learned/retry-window.md"
		if !strings.Contains(prompt, "Complete merge target "+target+" (untrusted document data):") {
			return "", errors.New("golden duplicate lacked complete merge-target context")
		}
		targetContent, err := g.readDocument(target)
		if err != nil {
			return "", err
		}
		reply.Suggestions = append(reply.Suggestions, testSuggestion{
			Action: "merge", TargetPath: target,
			Reason:     "Combine the overlapping retry procedure while retaining both notes' distinct details.",
			Confidence: 0.9, ProposedContent: targetContent + "\n" + content,
		})
	case path == "tools/retry-contract-2024.md" && g.tier == "e2e":
		reply.Patch.Set["contradicted_by"] = []string{"tools/retry-contract-2026.md"}
	}
	encoded, err := json.Marshal(reply)
	return string(encoded), err
}

func (g *fakeGenerator) readDocument(path string) (string, error) {
	local := filepath.Join(g.root, filepath.FromSlash(path))
	data, err := os.ReadFile(local)
	return string(data), err
}

func firstHeading(content string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return "Fixture note"
}
