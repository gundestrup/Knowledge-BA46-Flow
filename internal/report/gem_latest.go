package report

import (
	"context"
	"net/url"
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

type rubygemsVersion struct {
	Number     string `json:"number"`
	CreatedAt  string `json:"created_at"`
	Prerelease bool   `json:"prerelease"`
}

func refreshGemLatest(
	ctx context.Context,
	client *remoteHTTP,
	installed map[string]map[string][]string,
	existing map[string]GemLatest,
	force bool,
) map[string]GemLatest {
	result := make(map[string]GemLatest, len(existing)+len(installed))
	for name, cached := range existing {
		result[name] = cached
	}
	now := time.Now().UTC()
	names := make([]string, 0, len(installed))
	for name := range installed {
		cached, ok := existing[name]
		if force || !ok || cacheStale(cached, now) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	if len(names) == 0 {
		return result
	}
	var group errgroup.Group
	var mu sync.Mutex
	group.SetLimit(min(8, len(names)))
	for _, name := range names {
		if ctx.Err() != nil {
			break
		}
		group.Go(func() error {
			if ctx.Err() != nil {
				return nil
			}
			entry, err := fetchGemLatest(ctx, client, name, now)
			if err != nil {
				return nil
			}
			mu.Lock()
			result[name] = entry
			mu.Unlock()
			return nil
		})
	}
	_ = group.Wait()
	return result
}

func cacheStale(entry GemLatest, now time.Time) bool {
	if entry.CheckedAt == "" {
		return true
	}
	checked, err := time.Parse(time.RFC3339, entry.CheckedAt)
	return err != nil || now.Sub(checked) > gemLatestTTL
}

func fetchGemLatest(ctx context.Context, client *remoteHTTP, name string, now time.Time) (GemLatest, error) {
	target := client.rubygemsBase + "/" + url.PathEscape(name) + ".json"
	var versions []rubygemsVersion
	if err := client.getJSON(ctx, target, requestAuth{}, &versions); err != nil {
		return GemLatest{}, err
	}
	var highest, newest *rubygemsVersion
	for i := range versions {
		version := &versions[i]
		if version.Prerelease {
			continue
		}
		highest = higherGemRelease(highest, version)
		if newest == nil || releaseAfter(version.CreatedAt, newest.CreatedAt) {
			newest = version
		}
	}
	entry := GemLatest{CheckedAt: now.Format(time.RFC3339)}
	if highest != nil {
		entry.Version = highest.Number
	}
	if newest != nil {
		entry.Released = newest.CreatedAt
	}
	return entry, nil
}

// higherGemRelease returns whichever of current/candidate carries the higher
// gem version. Entries whose numbers fail to parse never displace the current
// highest.
func higherGemRelease(current, candidate *rubygemsVersion) *rubygemsVersion {
	if current == nil {
		return candidate
	}
	left, lok := parseGemVersion(candidate.Number)
	right, rok := parseGemVersion(current.Number)
	if lok && rok && compareGemVersions(left, right) > 0 {
		return candidate
	}
	return current
}

func releaseAfter(left, right string) bool {
	leftTime, leftErr := time.Parse(time.RFC3339, left)
	rightTime, rightErr := time.Parse(time.RFC3339, right)
	if leftErr != nil {
		return false
	}
	return rightErr != nil || leftTime.After(rightTime)
}
