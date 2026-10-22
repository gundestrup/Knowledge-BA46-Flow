package report

import (
	"context"
	"encoding/json"
	"strconv"
	"time"
)

const nodeReleaseTTL = 24 * time.Hour

type nodeReleaseRecord struct {
	Version string          `json:"version"`
	LTS     json.RawMessage `json:"lts"`
}

func refreshNodeReleaseCache(
	ctx context.Context,
	client *remoteHTTP,
	existing NodeReleaseCache,
	force bool,
) NodeReleaseCache {
	now := time.Now().UTC()
	if !force && existing.FailedAt != "" {
		if failed, err := time.Parse(time.RFC3339, existing.FailedAt); err == nil && now.Sub(failed) < time.Hour {
			return existing
		}
	}
	if !force && !existing.Stale && existing.CheckedAt != "" {
		if checked, err := time.Parse(time.RFC3339, existing.CheckedAt); err == nil &&
			now.Sub(checked) <= nodeReleaseTTL {
			return existing
		}
	}
	var releases []nodeReleaseRecord
	if err := client.getJSONAccept(ctx, client.nodeDistIndex, requestAuth{}, "application/json", &releases); err != nil {
		existing.Stale = true
		existing.FailedAt = now.Format(time.RFC3339)
		return existing
	}
	cache := NodeReleaseCache{
		ByMajor:   map[string]NodeRelease{},
		CheckedAt: now.Format(time.RFC3339),
	}
	for _, release := range releases {
		version, ok := parseNPMVersion(release.Version)
		if !ok || version.version.IsPreRelease() {
			continue
		}
		major := uintString(version.major)
		current, found := cache.ByMajor[major]
		currentVersion, currentOK := parseNPMVersion(current.Version)
		if found && currentOK && compareNPMVersions(version, currentVersion) <= 0 {
			continue
		}
		lts := ""
		if len(release.LTS) > 0 && string(release.LTS) != "false" && string(release.LTS) != "null" {
			_ = json.Unmarshal(release.LTS, &lts)
		}
		cache.ByMajor[major] = NodeRelease{Version: npmVersionString(version), LTS: lts}
	}
	if len(cache.ByMajor) == 0 {
		existing.Stale = true
		existing.FailedAt = now.Format(time.RFC3339)
		return existing
	}
	return cache
}

func uintString(value uint64) string {
	return strconv.FormatUint(value, 10)
}
