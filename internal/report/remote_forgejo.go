package report

import (
	"context"
	"net/url"
	"strings"
)

type forgejoTag struct {
	Name string `json:"name"`
}

type forgejoRelease struct {
	TagName string `json:"tag_name"`
}

type forgejoTasks struct {
	Total int `json:"total_count"`
}

func (h *remoteHTTP) forgejoInfo(ctx context.Context, base, owner, name string, auth requestAuth) *ForgejoInfo {
	root := strings.TrimRight(base, "/") + "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
	info := &ForgejoInfo{}

	var pulls []pull
	if h.getJSON(ctx, root+"/pulls?state=open&limit=50", auth, &pulls) == nil {
		count, titles, links := pullFields(pulls)
		info.OpenPRs, info.PRTitles, info.PRs = &count, &titles, &links
	}
	var tagNames, releaseNames *[]string
	var tags []forgejoTag
	if h.getJSON(ctx, root+"/tags?limit=100", auth, &tags) == nil {
		names := make([]string, 0, len(tags))
		for _, tag := range tags {
			names = append(names, tag.Name)
		}
		tagNames = &names
	}
	var releases []forgejoRelease
	if h.getJSON(ctx, root+"/releases?limit=100", auth, &releases) == nil {
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
	var tasks forgejoTasks
	if h.getJSON(ctx, root+"/actions/tasks?limit=1", auth, &tasks) == nil {
		info.ActionRuns = &tasks.Total
	}
	return info
}

func carryForgejoStale(current, previous *ForgejoInfo) *ForgejoInfo {
	if current == nil {
		current = &ForgejoInfo{}
	}
	if previous == nil {
		return current
	}
	stale := []string{}
	carryRemoteField(&current.OpenPRs, previous.OpenPRs, "open_prs", &stale)
	carryRemoteField(&current.PRTitles, previous.PRTitles, "pr_titles", &stale)
	carryRemoteField(&current.PRs, previous.PRs, "prs", &stale)
	carryRemoteField(&current.ActionRuns, previous.ActionRuns, "action_runs", &stale)
	carryRemoteField(&current.TagsWithoutRelease, previous.TagsWithoutRelease, "tags_without_release", &stale)
	carryRemoteField(&current.ReleasesWithoutTag, previous.ReleasesWithoutTag, "releases_without_tag", &stale)
	current.StaleFields = stale
	return current
}

func cloneForgejoInfo(source *ForgejoInfo) *ForgejoInfo {
	if source == nil {
		return nil
	}
	clone := *source
	clone.StaleFields = append([]string{}, source.StaleFields...)
	return &clone
}
