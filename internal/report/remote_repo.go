package report

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var sonarProjectKey = regexp.MustCompile(`sonar\.projectKey\s*=\s*(\S+)`)

type repoRemoteAddress struct {
	Host     string
	Hostname string
	Scheme   string
	Owner    string
	Name     string
}

func splitRepoRemote(raw string) (repoRemoteAddress, bool) {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	address := repoRemoteAddress{}
	path := ""
	if err == nil && parsed.Host != "" {
		address.Host = parsed.Host
		address.Hostname = parsed.Hostname()
		address.Scheme = parsed.Scheme
		path = parsed.Path
	} else {
		// Support Git's scp-style remotes such as git@github.com:org/repo.git.
		at := strings.LastIndex(raw, "@")
		colon := strings.Index(raw[at+1:], ":")
		if at < 0 || colon < 0 {
			return repoRemoteAddress{}, false
		}
		colon += at + 1
		address.Host = raw[at+1 : colon]
		address.Hostname = address.Host
		address.Scheme = "https"
		path = raw[colon+1:]
	}
	path = strings.Trim(strings.TrimSuffix(path, ".git"), "/")
	parts := strings.Split(path, "/")
	if address.Hostname == "" || len(parts) < 2 {
		return repoRemoteAddress{}, false
	}
	address.Owner = parts[0]
	address.Name = strings.Join(parts[1:], "/")
	return address, true
}

func (h *remoteHTTP) sonarStatus(ctx context.Context, dir, githubSlug string) *sonarResult {
	key := ""
	if data, err := os.ReadFile(filepath.Join(dir, "sonar-project.properties")); err == nil {
		if match := sonarProjectKey.FindSubmatch(data); match != nil {
			key = string(match[1])
		}
	}
	if key != "" {
		if result := h.sonarGate(ctx, h.sonarCloudBase, key); result != nil {
			result.URL = h.sonarCloudBase + "/project/overview?id=" + url.QueryEscape(key)
			result.Kind = "cloud"
			return result
		}
		if result := h.sonarGate(ctx, h.localSonarBase, key); result != nil {
			result.URL = h.localSonarBase + "/dashboard?id=" + url.QueryEscape(key)
			result.Kind = "local"
			return result
		}
		return nil
	}
	if githubSlug == "" {
		return nil
	}
	key = strings.ReplaceAll(githubSlug, "/", "_")
	result := h.sonarGate(ctx, h.sonarCloudBase, key)
	if result != nil {
		result.URL = h.sonarCloudBase + "/project/overview?id=" + url.QueryEscape(key)
		result.Kind = "autoscan"
	}
	return result
}

type sonarResult struct {
	Status   string
	Failures []string
	URL      string
	Kind     string
}

type sonarResponse struct {
	ProjectStatus struct {
		Status     string `json:"status"`
		Conditions []struct {
			Status         string `json:"status"`
			MetricKey      string `json:"metricKey"`
			ActualValue    string `json:"actualValue"`
			ErrorThreshold string `json:"errorThreshold"`
			Comparator     string `json:"comparator"`
		} `json:"conditions"`
	} `json:"projectStatus"`
}

func (h *remoteHTTP) sonarGate(ctx context.Context, host, key string) *sonarResult {
	query := url.Values{"projectKey": []string{key}}
	var response sonarResponse
	if err := h.getJSON(ctx, strings.TrimRight(host, "/")+"/api/qualitygates/project_status?"+query.Encode(),
		requestAuth{}, &response); err != nil || response.ProjectStatus.Status == "" {
		return nil
	}
	result := &sonarResult{Status: response.ProjectStatus.Status, Failures: []string{}}
	operators := map[string]string{"GT": ">", "LT": "<", "EQ": "=", "NE": "!="}
	for _, condition := range response.ProjectStatus.Conditions {
		if condition.Status != "ERROR" {
			continue
		}
		operator := operators[condition.Comparator]
		if operator == "" {
			operator = condition.Comparator
		}
		result.Failures = append(result.Failures,
			fmt.Sprintf("%s %s%s%s", condition.MetricKey, condition.ActualValue, operator, condition.ErrorThreshold))
	}
	return result
}

func refreshRemoteRepository(
	ctx context.Context,
	client *remoteHTTP,
	repo *Repository,
	dir string,
	entry repositoryEntry,
	old *Repository,
	ghToken string,
	unchanged bool,
) {
	address, hasAddress := splitRepoRemote(entry.URL)
	githubSlug := ""
	switch {
	case hasAddress && strings.EqualFold(address.Hostname, "github.com"):
		githubSlug = address.Owner + "/" + address.Name
		repo.Forgejo = nil
		var previous *GitHubInfo
		if old != nil {
			previous = old.GitHub
		}
		switch {
		case unchanged && previous != nil:
			repo.GitHub = cloneGitHubInfo(previous)
			repo.GitHub.AuthUnavailable = ghToken == ""
			if ghToken != "" {
				if ci := client.githubCI(ctx, address.Owner, address.Name, ghToken); ci != nil {
					repo.GitHub.CI = ci
					repo.GitHub.StaleFields = removeStaleField(repo.GitHub.StaleFields, "ci")
				}
			}
		case ghToken == "":
			repo.GitHub = carryGitHubStale(nil, previous)
			repo.GitHub.AuthUnavailable = true
		default:
			repo.GitHub = carryGitHubStale(client.githubInfo(ctx, address.Owner, address.Name, ghToken), previous)
			repo.GitHub.AuthUnavailable = false
		}
		if grade := client.codeFactor(ctx, address.Owner, address.Name); grade != nil {
			repo.CodeFactor = grade
			repo.CodeFactorStale = false
		} else if old != nil {
			repo.CodeFactor = old.CodeFactor
			repo.CodeFactorStale = old.CodeFactor != nil
		}
		branch := ""
		if repo.GitHub != nil && repo.GitHub.DefaultBranch != nil {
			branch = *repo.GitHub.DefaultBranch
		}
		if coverage, err := client.codecovCoverage(ctx, address.Owner, address.Name, branch); err != nil {
			if old != nil {
				repo.Codecov = old.Codecov
				repo.CodecovStale = old.Codecov != nil
			}
		} else {
			repo.Codecov = coverage
			repo.CodecovStale = false
		}
	case hasAddress && isForgejoHost(address.Host):
		repo.GitHub = nil
		repo.CodeFactor = nil
		repo.CodeFactorStale = false
		repo.Codecov = nil
		repo.CodecovStale = false
		var previous *ForgejoInfo
		if old != nil {
			previous = old.Forgejo
		}
		apiHost, trusted := configuredForgejoHost(address.Host)
		if !trusted {
			repo.Forgejo = carryForgejoStale(nil, previous)
		} else {
			base := "https://" + apiHost + "/api/v1"
			auth := client.forgejoCredentials(ctx, apiHost)
			switch {
			case unchanged && previous != nil:
				repo.Forgejo = cloneForgejoInfo(previous)
				repo.Forgejo.AuthUnavailable = auth.empty()
			case auth.empty():
				repo.Forgejo = carryForgejoStale(nil, previous)
				repo.Forgejo.AuthUnavailable = true
			default:
				repo.Forgejo = carryForgejoStale(client.forgejoInfo(ctx, base, address.Owner, address.Name, auth), previous)
				repo.Forgejo.AuthUnavailable = false
			}
		}
	default:
		repo.GitHub = nil
		repo.Forgejo = nil
		repo.CodeFactor = nil
		repo.CodeFactorStale = false
		repo.Codecov = nil
		repo.CodecovStale = false
	}

	if sonar := client.sonarStatus(ctx, dir, githubSlug); sonar != nil {
		repo.SonarCloud = stringPointer(sonar.Status)
		repo.SonarFailures = append([]string{}, sonar.Failures...)
		repo.SonarURL = sonar.URL
		repo.SonarKind = sonar.Kind
		repo.SonarStale = false
	} else if old != nil {
		repo.SonarCloud = old.SonarCloud
		repo.SonarFailures = old.SonarFailures
		repo.SonarURL = old.SonarURL
		repo.SonarKind = old.SonarKind
		repo.SonarStale = old.SonarCloud != nil
	}
}

func removeStaleField(fields []string, target string) []string {
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if field != target {
			out = append(out, field)
		}
	}
	return out
}

func stringPointer(value string) *string {
	return &value
}
