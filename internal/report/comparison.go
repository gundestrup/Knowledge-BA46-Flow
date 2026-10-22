package report

import (
	"encoding/json"
	"maps"
	"os"
)

// Comparison contains remote aggregates copied from the original Ruby report
// output for parity review. It is optional and read-only; live native
// collectors populate the repository fields separately.
type Comparison struct {
	Available               bool   `json:"available"`
	GeneratedAt             string `json:"generated_at,omitempty"`
	SonarReportedProjects   int    `json:"sonar_reported_projects"`
	SonarBlocked            *int   `json:"sonar_blocked"`
	SemgrepReportedProjects int    `json:"semgrep_reported_projects"`
	SemgrepBlocked          *int   `json:"semgrep_blocked"`
	SemgrepOpenFindings     *int   `json:"semgrep_open_findings"`
	OpenPRs                 *int   `json:"open_prs"`
	OpenPRRepositories      int    `json:"open_pr_repositories"`
	Error                   string `json:"error,omitempty"`
}

type comparisonFile struct {
	GeneratedAt string                    `json:"generated_at"`
	Repos       map[string]comparisonRepo `json:"repos"`
	GemLatest   map[string]GemLatest      `json:"gem_latest"`
}

type comparisonRepo struct {
	GitHub             *GitHubInfo       `json:"github"`
	Forgejo            *ForgejoInfo      `json:"forgejo"`
	CodeFactor         *string           `json:"codefactor"`
	SonarCloud         *string           `json:"sonarcloud"`
	SonarCloudFailures []string          `json:"sonarcloud_failures"`
	SonarCloudURL      string            `json:"sonarcloud_url"`
	SonarCloudKind     string            `json:"sonarcloud_kind"`
	SemgrepLicense     string            `json:"semgrep_license"`
	SemgrepFindings    *SemgrepFindings  `json:"semgrep_findings"`
	Gems               map[string]string `json:"gems"`
	GemChanges         GemChanges        `json:"gem_changes"`
}

func loadComparison(path string) Comparison {
	if path == "" {
		return Comparison{Error: "not configured"}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Comparison{Error: "comparison report unavailable"}
	}
	var source comparisonFile
	if err := json.Unmarshal(data, &source); err != nil || source.Repos == nil {
		return Comparison{Error: "comparison report is invalid"}
	}

	result := Comparison{
		Available:   true,
		GeneratedAt: source.GeneratedAt,
	}
	semgrepTotal := 0
	semgrepBlocked := 0
	hasSemgrepCount := false
	sonarBlocked := 0
	hasSonarCount := false
	prTotal := 0
	for _, repo := range source.Repos {
		if repo.SonarCloud != nil || len(repo.SonarCloudFailures) > 0 {
			result.SonarReportedProjects++
			hasSonarCount = true
			if repo.SonarCloud == nil || *repo.SonarCloud != "OK" || len(repo.SonarCloudFailures) > 0 {
				sonarBlocked++
			}
		}
		if repo.SemgrepFindings != nil && repo.SemgrepFindings.Open != nil {
			result.SemgrepReportedProjects++
			semgrepTotal += *repo.SemgrepFindings.Open
			if *repo.SemgrepFindings.Open > 0 {
				semgrepBlocked++
			}
			hasSemgrepCount = true
		}
		for _, remote := range []*GitHubInfo{repo.GitHub} {
			if remote == nil || remote.OpenPRs == nil {
				continue
			}
			prTotal += *remote.OpenPRs
			result.OpenPRRepositories++
		}
		for _, remote := range []*ForgejoInfo{repo.Forgejo} {
			if remote == nil || remote.OpenPRs == nil {
				continue
			}
			prTotal += *remote.OpenPRs
			result.OpenPRRepositories++
		}
	}
	if hasSemgrepCount {
		result.SemgrepOpenFindings = &semgrepTotal
		result.SemgrepBlocked = &semgrepBlocked
	}
	if hasSonarCount {
		result.SonarBlocked = &sonarBlocked
	}
	if result.OpenPRRepositories > 0 {
		result.OpenPRs = &prTotal
	}
	return result
}

func loadComparisonGemLatest(path string) map[string]GemLatest {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var source comparisonFile
	if json.Unmarshal(data, &source) != nil {
		return nil
	}
	return source.GemLatest
}

func loadComparisonRepositories(path string) map[string]*Repository {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var source comparisonFile
	if json.Unmarshal(data, &source) != nil {
		return nil
	}
	repos := make(map[string]*Repository, len(source.Repos))
	for name, old := range source.Repos {
		github := cloneGitHubInfo(old.GitHub)
		if github != nil && github.PRs != nil {
			pulls := append([]PullRequest{}, (*github.PRs)...)
			for i := range pulls {
				pulls[i].URL = safeHTTPLink(pulls[i].URL)
			}
			github.PRs = &pulls
		}
		forgejo := cloneForgejoInfo(old.Forgejo)
		if forgejo != nil && forgejo.PRs != nil {
			pulls := append([]PullRequest{}, (*forgejo.PRs)...)
			for i := range pulls {
				pulls[i].URL = safeHTTPLink(pulls[i].URL)
			}
			forgejo.PRs = &pulls
		}
		sonarURL := safeHTTPLink(old.SonarCloudURL)
		findings := old.SemgrepFindings
		if findings != nil {
			copy := *findings
			copy.URL = safeHTTPLink(copy.URL)
			copy.Severities = maps.Clone(copy.Severities)
			findings = &copy
		}
		repos[name] = &Repository{
			Name:            name,
			GitHub:          github,
			Forgejo:         forgejo,
			CodeFactor:      old.CodeFactor,
			SonarCloud:      old.SonarCloud,
			SonarFailures:   old.SonarCloudFailures,
			SonarURL:        sonarURL,
			SonarKind:       old.SonarCloudKind,
			SemgrepLicense:  old.SemgrepLicense,
			SemgrepFindings: findings,
			Gems:            old.Gems,
			GemChanges:      old.GemChanges,
		}
	}
	return repos
}
