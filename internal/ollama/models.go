package ollama

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/sync/errgroup"
)

const (
	CapabilityCompletion = "completion"
	CapabilityEmbedding  = "embedding"
)

// Model describes a model registered on the configured server, not the online
// library. Unknown capabilities/locality are never inferred to be safe.
type Model struct {
	Name          string   `json:"name"`
	Digest        string   `json:"digest,omitempty"`
	Size          int64    `json:"size,omitempty"`
	Capabilities  []string `json:"capabilities"`
	Locality      string   `json:"locality"`
	ContextLength int      `json:"context_length,omitempty"`
	Error         string   `json:"error,omitempty"`
}

func (m Model) Supports(capability string) bool {
	return slices.Contains(m.Capabilities, capability)
}

func (m Model) Validate(capability string, allowRemote bool) error {
	if !m.Supports(capability) {
		return fmt.Errorf("model %q does not advertise the %q capability", m.Name, capability)
	}
	if m.Locality != "local" && !allowRemote {
		return fmt.Errorf("model %q is %s; explicit cloud/remote consent is required", m.Name, m.Locality)
	}
	return nil
}

// RemoteServer deliberately avoids DNS lookups: only literal loopback hosts and
// localhost are considered on-machine.
func RemoteServer(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return true
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	ip := net.ParseIP(host)
	return host != "localhost" && (ip == nil || !ip.IsLoopback())
}

// CloudName is only a conservative privacy guard, never a capability detector.
func CloudName(name string) bool {
	tag := name[strings.LastIndex(name, ":")+1:]
	return tag == "cloud" || strings.HasSuffix(tag, "-cloud")
}

func CanonicalModelName(name string) string {
	if strings.LastIndex(name, ":") <= strings.LastIndex(name, "/") {
		return name + ":latest"
	}
	return name
}

func (c *Client) ListModels(ctx context.Context) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/tags", nil)
	if err != nil {
		return nil, fmt.Errorf("build model-list request: %w", err)
	}
	var response struct {
		Models []Model `json:"models"`
		Error  string  `json:"error"`
	}
	if err := c.doJSON(req, &response); err != nil {
		return nil, err
	}
	if response.Error != "" {
		return nil, errors.New(response.Error)
	}
	if len(response.Models) > 256 {
		return nil, errors.New("ollama model list exceeds 256 entries")
	}
	models := []Model{}
	for _, model := range response.Models {
		if model.Name != "" && !slices.ContainsFunc(models, func(m Model) bool { return m.Name == model.Name }) {
			model.Capabilities = []string{}
			model.Locality = "unknown"
			models = append(models, model)
		}
	}
	slices.SortFunc(models, func(a, b Model) int { return strings.Compare(a.Name, b.Name) })
	return models, nil
}

func (c *Client) ShowModel(ctx context.Context, name string) (Model, error) {
	var response struct {
		Capabilities []string       `json:"capabilities"`
		ModelInfo    map[string]any `json:"model_info"`
		RemoteHost   string         `json:"remote_host"`
		RemoteModel  string         `json:"remote_model"`
		Error        string         `json:"error"`
	}
	if err := c.post(ctx, "/api/show", map[string]string{"model": name}, &response); err != nil {
		return Model{}, err
	}
	if response.Error != "" {
		return Model{}, errors.New(response.Error)
	}
	model := Model{Name: name, Capabilities: append([]string{}, response.Capabilities...), Locality: "unknown"}
	switch {
	case response.RemoteHost != "" || response.RemoteModel != "" || CloudName(name):
		model.Locality = "cloud"
	case RemoteServer(c.base):
		model.Locality = "remote"
	case len(response.ModelInfo) > 0:
		model.Locality = "local"
	}
	for key, value := range response.ModelInfo {
		if strings.HasSuffix(key, ".context_length") {
			if n, ok := value.(float64); ok && n > 0 && n < 1<<31 && int(n) > model.ContextLength {
				model.ContextLength = int(n)
			}
		}
	}
	return model, nil
}

// DiscoverModels performs metadata-only queries with bounded concurrency. A
// failed show leaves that entry unverified instead of guessing its capabilities.
func (c *Client) DiscoverModels(ctx context.Context) ([]Model, error) {
	models, err := c.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	var group errgroup.Group
	group.SetLimit(4)
	for i := range models {
		group.Go(func() error {
			info, err := c.ShowModel(ctx, models[i].Name)
			if err != nil {
				models[i].Error = err.Error()
				return nil
			}
			info.Digest, info.Size = models[i].Digest, models[i].Size
			models[i] = info
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return models, nil
}

// WithModelCheck validates capabilities and privacy before sending content.
// It is lazy so a no-op pass still works without a running Ollama server.
func (c *Client) WithModelCheck(capability string, allowRemote bool) *Client {
	c.requiredCapability = capability
	c.allowRemote = allowRemote
	return c
}

func (c *Client) checkModel(ctx context.Context, capability string) error {
	if c.requiredCapability == "" {
		return nil
	}
	if c.requiredCapability != capability {
		return fmt.Errorf("client requires %q, not %q", c.requiredCapability, capability)
	}
	c.checkOnce.Do(func() {
		info, err := c.ShowModel(ctx, c.model)
		if err == nil {
			err = info.Validate(capability, c.allowRemote)
		}
		c.checkErr = err
		if err == nil && info.ContextLength > 0 {
			c.ctxOnce.Do(func() { c.ctxLen = info.ContextLength })
		}
	})
	return c.checkErr
}
