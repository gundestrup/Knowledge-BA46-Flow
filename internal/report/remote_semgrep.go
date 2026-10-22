package report

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

type semgrepDeploymentList struct {
	Deployments []struct {
		Slug string `json:"slug"`
	} `json:"deployments"`
}

type semgrepProject struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type semgrepProjectList struct {
	Projects []semgrepProject `json:"projects"`
}

type semgrepFinding struct {
	Severity   string `json:"severity"`
	Repository struct {
		Name string `json:"name"`
	} `json:"repository"`
}

type semgrepFindingList struct {
	Findings []semgrepFinding `json:"findings"`
	SAST     struct {
		Findings []semgrepFinding `json:"findings"`
	} `json:"sastFindings"`
}

type semgrepData struct {
	Slug     string
	Projects []semgrepProject
	Findings []semgrepFinding
}

func (h *remoteHTTP) semgrepPlatform(ctx context.Context, token string) (*semgrepData, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("semgrep token is not configured")
	}
	base := strings.TrimRight(h.semgrepBase, "/")
	var deployments semgrepDeploymentList
	if err := h.getJSON(ctx, base+"/deployments", requestAuth{bearer: token}, &deployments); err != nil {
		return nil, err
	}
	if len(deployments.Deployments) == 0 || deployments.Deployments[0].Slug == "" {
		return nil, fmt.Errorf("semgrep deployment is unavailable")
	}
	data := &semgrepData{Slug: deployments.Deployments[0].Slug}
	deployment := url.PathEscape(data.Slug)
	for page := 0; ; page++ {
		target := fmt.Sprintf("%s/deployments/%s/projects?page=%d&page_size=100", base, deployment, page)
		var projects semgrepProjectList
		if err := h.getJSON(ctx, target, requestAuth{bearer: token}, &projects); err != nil {
			return nil, err
		}
		data.Projects = append(data.Projects, projects.Projects...)
		if len(projects.Projects) < 100 {
			break
		}
	}
	for page := 0; ; page++ {
		target := fmt.Sprintf(
			"%s/deployments/%s/findings?issue_type=sast&status=open&dedup=true&page_size=3000&page=%d",
			base, deployment, page,
		)
		var findings semgrepFindingList
		if err := h.getJSON(ctx, target, requestAuth{bearer: token}, &findings); err != nil {
			return nil, err
		}
		rows := findings.Findings
		if rows == nil {
			rows = findings.SAST.Findings
		}
		data.Findings = append(data.Findings, rows...)
		if len(rows) < 3000 {
			break
		}
	}
	return data, nil
}

func refreshSemgrepFindings(data *semgrepData, repos []Repository, registry map[string]repositoryEntry) {
	projectsByRepo := make(map[string]semgrepProject, len(repos))
	for _, repo := range repos {
		entry := registry[repo.Name]
		desiredName := entry.SemgrepRepo
		if desiredName == "" {
			desiredName = repoSlug(entry.URL)
		}
		urlKey := normalizedRepoURL(entry.URL)
		for _, project := range data.Projects {
			if project.Name == desiredName ||
				(project.URL != "" && normalizedRepoURL(project.URL) == urlKey) {
				projectsByRepo[repo.Name] = project
				break
			}
		}
	}
	for i := range repos {
		repo := &repos[i]
		project, found := projectsByRepo[repo.Name]
		if !found {
			if repo.SemgrepLicense == "cloud-ci" {
				repo.SemgrepFindings = &SemgrepFindings{}
			}
			continue
		}
		count := 0
		severities := map[string]int{}
		for _, finding := range data.Findings {
			if finding.Repository.Name != project.Name {
				continue
			}
			count++
			severity := strings.ToLower(finding.Severity)
			severities[severity]++
		}
		projectName := project.Name
		repo.SemgrepFindings = &SemgrepFindings{
			Project:    &projectName,
			Open:       &count,
			Severities: severities,
			URL:        "https://semgrep.dev/orgs/" + url.PathEscape(data.Slug) + "/findings?repo=" + url.QueryEscape(project.Name),
		}
	}
}

func repoSlug(raw string) string {
	address, ok := splitRepoRemote(raw)
	if !ok {
		return ""
	}
	return address.Owner + "/" + address.Name
}

func normalizedRepoURL(raw string) string {
	raw = strings.TrimSpace(raw)
	address, ok := splitRepoRemote(raw)
	if !ok {
		return strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(raw, "/"), ".git"))
	}
	return strings.ToLower("https://" + address.Hostname + "/" + strings.TrimSuffix(address.Owner+"/"+address.Name, ".git"))
}
