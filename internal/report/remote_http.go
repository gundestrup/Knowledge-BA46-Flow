package report

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"
	"resty.dev/v3"
)

const (
	githubAPIBase      = "https://api.github.com"
	sonarCloudBase     = "https://sonarcloud.io"
	semgrepAPIBase     = "https://semgrep.dev/api/v1"
	defaultForgejoHost = "forgejo.borg-collective.eu"
)

type remoteHTTP struct {
	client            *resty.Client
	githubBase        string
	sonarCloudBase    string
	localSonarBase    string
	semgrepBase       string
	rubygemsBase      string
	npmRegistryBase   string
	nodeDistIndex     string
	endOfLifeBase     string
	codeFactorBase    string
	codecovBase       string
	authMu            sync.Mutex
	forgejoAuthByHost map[string]requestAuth
}

type requestAuth struct {
	token    string
	bearer   string
	username string
	password string
}

func (a requestAuth) empty() bool {
	return a.token == "" && a.bearer == "" && a.username == "" && a.password == ""
}

func newRemoteHTTP() *remoteHTTP {
	return &remoteHTTP{
		client: resty.New().
			SetTimeout(15 * time.Second).
			SetResponseBodyLimit(20 << 20),
		githubBase:      githubAPIBase,
		sonarCloudBase:  sonarCloudBase,
		localSonarBase:  "http://127.0.0.1:9000",
		semgrepBase:     semgrepAPIBase,
		rubygemsBase:    "https://rubygems.org/api/v1/versions",
		npmRegistryBase: "https://registry.npmjs.org",
		nodeDistIndex:   "https://nodejs.org/dist/index.json",
		endOfLifeBase:   "https://endoflife.date/api",
		codeFactorBase:  "https://www.codefactor.io",
		codecovBase:     "https://api.codecov.io",
	}
}

// request builds a GET request with context, Accept, and the auth variant the
// target API expects: Bearer, token, or Basic.
func (h *remoteHTTP) request(ctx context.Context, auth requestAuth, accept string) *resty.Request {
	req := h.client.R().SetContext(ctx)
	if accept != "" {
		req.SetHeader("Accept", accept)
	}
	switch {
	case auth.bearer != "":
		req.SetHeader("Authorization", "Bearer "+auth.bearer)
	case auth.token != "":
		req.SetHeader("Authorization", "token "+auth.token)
	case auth.username != "" || auth.password != "":
		req.SetBasicAuth(auth.username, auth.password)
	}
	return req
}

func (h *remoteHTTP) get(ctx context.Context, target string, auth requestAuth, accept string) ([]byte, error) {
	response, err := h.request(ctx, auth, accept).Get(target)
	if err != nil {
		return nil, err
	}
	if response.IsStatusFailure() {
		return nil, fmt.Errorf("remote HTTP %d", response.StatusCode())
	}
	return response.Bytes(), nil
}

func (h *remoteHTTP) getJSON(ctx context.Context, target string, auth requestAuth, out any) error {
	return h.getJSONAccept(ctx, target, auth, "application/vnd.github+json, application/json", out)
}

func (h *remoteHTTP) getJSONAccept(ctx context.Context, target string, auth requestAuth, accept string, out any) error {
	body, err := h.get(ctx, target, auth, accept)
	if err != nil {
		return err
	}
	if !json.Valid(body) {
		return fmt.Errorf("remote response was not valid JSON")
	}
	return json.Unmarshal(body, out)
}

func safeHTTPLink(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return ""
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ""
	}
	return parsed.String()
}

func githubToken(ctx context.Context) string {
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(key)); token != "" {
			return token
		}
	}
	cmd := exec.CommandContext(ctx, "gh", "auth", "token") // NOSONAR — user-local CLI resolves gh/git via PATH by design
	output, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func forgejoAuth(ctx context.Context, host string) requestAuth {
	if token := strings.TrimSpace(os.Getenv("FORGEJO_TOKEN")); token != "" {
		return requestAuth{token: token}
	}
	input := "protocol=https\nhost=" + host + "\n\n"
	cmd := exec.CommandContext(ctx, "git", "credential", "fill") // NOSONAR — user-local CLI resolves gh/git via PATH by design
	cmd.Stdin = strings.NewReader(input)
	output, err := cmd.Output()
	if err != nil {
		return requestAuth{}
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			values[key] = value
		}
	}
	return requestAuth{username: values["username"], password: values["password"]}
}

func configuredForgejoHost(remoteHost string) (string, bool) {
	expected := strings.TrimSpace(os.Getenv("FORGEJO_HOST"))
	if expected == "" {
		expected = defaultForgejoHost
	}
	expected = strings.TrimPrefix(strings.TrimPrefix(expected, "https://"), "http://")
	expected = strings.TrimSuffix(expected, "/")
	return expected, strings.EqualFold(remoteHost, expected)
}

func isForgejoHost(host string) bool {
	if strings.Contains(strings.ToLower(host), "forgejo.") {
		return true
	}
	_, trusted := configuredForgejoHost(host)
	return trusted
}

func (h *remoteHTTP) forgejoCredentials(ctx context.Context, host string) requestAuth {
	h.authMu.Lock()
	defer h.authMu.Unlock()
	if auth, ok := h.forgejoAuthByHost[host]; ok {
		return auth
	}
	auth := forgejoAuth(ctx, host)
	if h.forgejoAuthByHost == nil {
		h.forgejoAuthByHost = map[string]requestAuth{}
	}
	h.forgejoAuthByHost[host] = auth
	return auth
}

// codecovToken returns the optional CODECOV_TOKEN. Public repositories report
// coverage without authentication; private repositories need a token with API
// access.
func codecovToken() string {
	return strings.TrimSpace(os.Getenv("CODECOV_TOKEN"))
}

func semgrepToken() string {
	if token := strings.TrimSpace(os.Getenv("SEMGREP_APP_TOKEN")); token != "" {
		return token
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".semgrep", "settings.yml"))
	if err != nil {
		return ""
	}
	var settings struct {
		APIToken string `yaml:"api_token"`
	}
	if yaml.Unmarshal(data, &settings) != nil {
		return ""
	}
	return strings.TrimSpace(settings.APIToken)
}
