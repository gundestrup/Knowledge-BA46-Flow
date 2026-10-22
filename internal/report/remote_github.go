package report

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var codeFactorGrade = regexp.MustCompile(`<text[^<>]*>([A-F][+-]?)</text>`)

type githubRepository struct {
	Topics        []string `json:"topics"`
	Private       bool     `json:"private"`
	DefaultBranch string   `json:"default_branch"`
}

// pull is the shared open-pull-request shape — GitHub and Forgejo expose the
// same fields, so one struct serves both collectors.
type pull struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	HTMLURL string `json:"html_url"`
}

// pullFields converts a pull list into the shared report fields: total count
// plus title/link lists capped at five entries.
func pullFields(pulls []pull) (count int, titles []string, links []PullRequest) {
	count = len(pulls)
	titles = make([]string, 0, min(5, count))
	links = make([]PullRequest, 0, min(5, count))
	for _, p := range pulls[:min(5, count)] {
		titles = append(titles, p.Title)
		links = append(links, PullRequest{Number: p.Number, Title: p.Title, URL: safeHTTPLink(p.HTMLURL)})
	}
	return count, titles, links
}

type githubWorkflowRuns struct {
	Runs []struct {
		Name       string `json:"name"`
		Conclusion string `json:"conclusion"`
		Status     string `json:"status"`
	} `json:"workflow_runs"`
}

type githubTag struct {
	Name string `json:"name"`
}

type githubRelease struct {
	TagName string `json:"tag_name"`
}

func (h *remoteHTTP) githubInfo(ctx context.Context, owner, name, token string) *GitHubInfo {
	base := strings.TrimRight(h.githubBase, "/") + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	auth := requestAuth{bearer: token}
	info := &GitHubInfo{}

	var meta githubRepository
	if h.getJSON(ctx, base, auth, &meta) == nil {
		topics := append([]string{}, meta.Topics...)
		private := meta.Private
		info.Topics, info.Private = &topics, &private
		if meta.DefaultBranch != "" {
			branch := meta.DefaultBranch
			info.DefaultBranch = &branch
		}
	}
	var pulls []pull
	if h.getJSON(ctx, base+"/pulls?state=open&per_page=100", auth, &pulls) == nil {
		count, titles, links := pullFields(pulls)
		info.OpenPRs, info.PRTitles, info.PRs = &count, &titles, &links
	}
	info.CI = h.githubCI(ctx, owner, name, token)
	var tagNames, releaseNames *[]string
	var tags []githubTag
	if h.getJSON(ctx, base+"/tags?per_page=100", auth, &tags) == nil {
		names := make([]string, 0, len(tags))
		for _, tag := range tags {
			names = append(names, tag.Name)
		}
		tagNames = &names
	}
	var releases []githubRelease
	if h.getJSON(ctx, base+"/releases?per_page=100", auth, &releases) == nil {
		names := make([]string, 0, len(releases))
		for _, release := range releases {
			names = append(names, release.TagName)
		}
		releaseNames = &names
	}
	if tagNames != nil && releaseNames != nil {
		info.TagsWithoutRelease = stringDifference(*tagNames, *releaseNames)
		info.ReleasesWithoutTag = stringDifference(*releaseNames, *tagNames)
	}
	var codeScanning []json.RawMessage
	if h.getJSON(ctx, base+"/code-scanning/alerts?state=open&per_page=100", auth, &codeScanning) == nil {
		count := len(codeScanning)
		info.CodeScanning = &count
	}
	var dependabot []json.RawMessage
	if h.getJSON(ctx, base+"/dependabot/alerts?state=open&per_page=100", auth, &dependabot) == nil {
		count := len(dependabot)
		info.Dependabot = &count
	}
	return info
}

func (h *remoteHTTP) githubCI(ctx context.Context, owner, name, token string) *string {
	base := strings.TrimRight(h.githubBase, "/") + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	var runs githubWorkflowRuns
	if h.getJSON(ctx, base+"/actions/runs?per_page=1", requestAuth{bearer: token}, &runs) != nil ||
		len(runs.Runs) == 0 {
		return nil
	}
	run := runs.Runs[0]
	state := run.Conclusion
	if state == "" {
		state = run.Status
	}
	value := run.Name + ":" + state
	return &value
}

func stringDifference(left, right []string) *[]string {
	set := make(map[string]bool, len(right))
	for _, value := range right {
		set[value] = true
	}
	difference := make([]string, 0, len(left))
	for _, value := range left {
		if !set[value] {
			difference = append(difference, value)
		}
	}
	return &difference
}

func (h *remoteHTTP) codeFactor(ctx context.Context, owner, name string) *string {
	target := fmt.Sprintf("%s/repository/github/%s/%s/badge", strings.TrimRight(h.codeFactorBase, "/"),
		url.PathEscape(owner), url.PathEscape(name))
	body, err := h.get(ctx, target, requestAuth{}, "text/html")
	if err != nil {
		return nil
	}
	match := codeFactorGrade.FindSubmatch(body)
	if match == nil {
		return nil
	}
	grade := string(match[1])
	return &grade
}

// codecovCoverage returns the totals.coverage percentage reported by the
// Codecov v2 API for the given branch (the repository's default branch when
// known). A nil coverage with a nil error means Codecov tracked the repo but
// reported no totals; an error means the lookup failed or Codecov does not
// know the repository — callers keep the previous value flagged stale.
func (h *remoteHTTP) codecovCoverage(ctx context.Context, owner, name, branch string) (*float64, error) {
	target := fmt.Sprintf("%s/api/v2/github/%s/repos/%s/totals/", strings.TrimRight(h.codecovBase, "/"),
		url.PathEscape(owner), url.PathEscape(name))
	if branch != "" {
		target += "?branch=" + url.QueryEscape(branch)
	}
	var payload struct {
		Totals struct {
			Coverage *float64 `json:"coverage"`
		} `json:"totals"`
	}
	if err := h.getJSON(ctx, target, requestAuth{bearer: codecovToken()}, &payload); err != nil {
		return nil, err
	}
	return payload.Totals.Coverage, nil
}

func carryGitHubStale(current, previous *GitHubInfo) *GitHubInfo {
	if current == nil {
		current = &GitHubInfo{}
	}
	if previous == nil {
		return current
	}
	stale := []string{}
	carryRemoteField(&current.Topics, previous.Topics, "topics", &stale)
	carryRemoteField(&current.Private, previous.Private, "private", &stale)
	carryRemoteField(&current.OpenPRs, previous.OpenPRs, "open_prs", &stale)
	carryRemoteField(&current.PRTitles, previous.PRTitles, "pr_titles", &stale)
	carryRemoteField(&current.PRs, previous.PRs, "prs", &stale)
	carryRemoteField(&current.CI, previous.CI, "ci", &stale)
	carryRemoteField(&current.TagsWithoutRelease, previous.TagsWithoutRelease, "tags_without_release", &stale)
	carryRemoteField(&current.ReleasesWithoutTag, previous.ReleasesWithoutTag, "releases_without_tag", &stale)
	carryRemoteField(&current.CodeScanning, previous.CodeScanning, "code_scanning", &stale)
	carryRemoteField(&current.Dependabot, previous.Dependabot, "dependabot", &stale)
	carryRemoteField(&current.DefaultBranch, previous.DefaultBranch, "default_branch", &stale)
	current.StaleFields = stale
	return current
}

func cloneGitHubInfo(source *GitHubInfo) *GitHubInfo {
	if source == nil {
		return nil
	}
	clone := *source
	clone.StaleFields = append([]string{}, source.StaleFields...)
	return &clone
}

func carryRemoteField[T any](current **T, previous *T, name string, stale *[]string) {
	if *current != nil || previous == nil {
		return
	}
	value := *previous
	*current = &value
	*stale = append(*stale, name)
}
