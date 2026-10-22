package report

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

// toolchainTTL mirrors nodeReleaseTTL: policy channels are re-resolved once a
// day, carried forward (stale) in between and on failure.
const toolchainTTL = 24 * time.Hour

// PolicyPin records how one versions.* policy entry was interpreted for this
// run: the configured expression, the concrete version it resolved to, and
// provenance. Channel expressions ("stable", "lts", "eol-floor") must always
// surface their resolved value so a finding can be traced to a version.
type PolicyPin struct {
	Policy    string `json:"policy"`
	Resolved  string `json:"resolved"`
	Source    string `json:"source,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
	Stale     bool   `json:"stale,omitempty"`
	Note      string `json:"note,omitempty"`
}

// policyChannels are resolved against upstream release data at report time
// rather than naming a concrete version.
var policyChannels = []string{"stable", "lts", "eol-floor"}

// toolchainProducts is the single table driving every per-product behavior:
// resolution backend (endoflife.date cycles for language toolchains,
// rubygems.org for the two Ruby package managers — whose channels other than
// "stable" have no meaning), and policy access via get/set so adding a
// product is one entry here plus one Versions field.
type toolchainProduct struct {
	field    string // policyFile.Versions field name — display + pin key
	cycleAPI string // endoflife.date product slug, "" when resolved via gems
	gem      string // rubygems.org gem name, "" when resolved via cycles
	// externalCheck marks products whose pins are evaluated by their own
	// routine (node → nodeVersionIssues/nearestNodeVersion) — they stay out
	// of the toolchain wanted map so one pin cannot produce two findings.
	externalCheck bool
	get           func(*policyFile) string
	set           func(*policyFile, string)
}

var toolchainProducts = []toolchainProduct{
	{field: "Ruby", cycleAPI: "ruby",
		get: func(p *policyFile) string { return p.Versions.Ruby },
		set: func(p *policyFile, v string) { p.Versions.Ruby = v }},
	{field: "Node", cycleAPI: "nodejs", externalCheck: true,
		get: func(p *policyFile) string { return p.Versions.Node },
		set: func(p *policyFile, v string) { p.Versions.Node = v }},
	{field: "Python", cycleAPI: "python",
		get: func(p *policyFile) string { return p.Versions.Python },
		set: func(p *policyFile, v string) { p.Versions.Python = v }},
	{field: "Go", cycleAPI: "go",
		get: func(p *policyFile) string { return p.Versions.Go },
		set: func(p *policyFile, v string) { p.Versions.Go = v }},
	{field: "Php", cycleAPI: "php",
		get: func(p *policyFile) string { return p.Versions.Php },
		set: func(p *policyFile, v string) { p.Versions.Php = v }},
	{field: "Bundler", gem: "bundler",
		get: func(p *policyFile) string { return p.Versions.Bundler },
		set: func(p *policyFile, v string) { p.Versions.Bundler = v }},
	{field: "Rubygems", gem: "rubygems-update",
		get: func(p *policyFile) string { return p.Versions.Rubygems },
		set: func(p *policyFile, v string) { p.Versions.Rubygems = v }},
}

// policyWanted builds the product → policy-expression map the version-file
// checks evaluate: built-ins via their accessor, custom keys normalized.
func policyWanted(policy policyFile) map[string]string {
	wanted := map[string]string{}
	for _, product := range toolchainProducts {
		if !product.externalCheck {
			wanted[normalizeProductName(product.field)] = product.get(&policy)
		}
	}
	for name, value := range policy.Versions.Custom {
		wanted[normalizeProductName(name)] = value
	}
	return wanted
}

// normalizeProductName canonicalizes product names for matching — policy
// keys, .tool-versions plugin names, and .<name>-version files.
func normalizeProductName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func isPolicyChannel(value string) bool {
	return slices.Contains(policyChannels, trimmedPolicy(value))
}

// applyResolvedVersions overlays resolved policy values onto a loaded policy.
// RunWithOptions passes the map from resolvePolicyVersions; nil leaves the
// policy untouched (channels then match leniently instead of flagging).
func applyResolvedVersions(policy *policyFile, resolved map[string]string) {
	for _, product := range toolchainProducts {
		value, ok := resolved[product.field]
		if !ok {
			continue
		}
		product.set(policy, value)
	}
	for name := range policy.Versions.Custom {
		if value, ok := resolved[name]; ok {
			policy.Versions.Custom[name] = value
		}
	}
}

func (p toolchainProduct) eolURL(client *remoteHTTP) string {
	return client.endOfLifeBase + "/" + url.PathEscape(p.cycleAPI) + ".json"
}

func (p toolchainProduct) gemURL(client *remoteHTTP) string {
	return client.rubygemsBase + "/" + url.PathEscape(p.gem) + ".json"
}

// resolvePolicyVersions produces the effective versions map used by the
// version-file checks plus the per-product PolicyPin audit trail. Literals
// pass through with resolved == policy. Channel expressions reuse a fresh
// (< toolchainTTL) previous resolution, refetch on remote runs, and otherwise
// carry the last known value forward marked stale. An unresolvable channel
// resolves to "" — checks treat that as "no policy" and the pin's note
// explains why instead of flagging every repository.
func resolvePolicyVersions(
	ctx context.Context,
	client *remoteHTTP,
	policy policyFile,
	previous map[string]PolicyPin,
	remote, force bool,
) (map[string]string, map[string]PolicyPin) {
	resolved := map[string]string{}
	pins := map[string]PolicyPin{}
	for _, product := range toolchainProducts {
		value := product.get(&policy)
		pin := resolveProductPin(ctx, client, product, value, previous[product.field], remote, force)
		pins[product.field] = pin
		resolved[product.field] = pin.Resolved
	}
	for name, value := range policy.Versions.Custom {
		product := customProduct(name)
		if product == nil {
			pins[name] = PolicyPin{Policy: trimmedPolicy(value),
				Note: "ignored — " + name + " is a built-in product name"}
			continue
		}
		pin := resolveProductPin(ctx, client, *product, value, previous[name], remote, force)
		pins[name] = pin
		resolved[name] = pin.Resolved
	}
	return resolved, pins
}

// customProduct reports whether a versions.custom key is usable — built-in
// product names are reserved so the two config paths cannot disagree.
func customProduct(name string) *toolchainProduct {
	normalized := normalizeProductName(name)
	for _, product := range toolchainProducts {
		if normalizeProductName(product.field) == normalized {
			return nil
		}
	}
	return &toolchainProduct{field: name, cycleAPI: normalized}
}

func resolveProductPin(
	ctx context.Context,
	client *remoteHTTP,
	product toolchainProduct,
	value string,
	previous PolicyPin,
	remote, force bool,
) PolicyPin {
	value = trimmedPolicy(value)
	if value == "" {
		return PolicyPin{}
	}
	if !isPolicyChannel(value) {
		return PolicyPin{Policy: value, Resolved: value}
	}
	if previous.Policy != value {
		previous = PolicyPin{} // a cached resolution belongs to its channel
	}
	now := time.Now().UTC()
	if !remote {
		// Local-only runs never fetch: like the other remote caches, carry the
		// last resolution forward and mark it stale for display.
		pin := previous
		pin.Policy = value
		if pin.Resolved != "" {
			pin.Stale = true
			return pin
		}
		return PolicyPin{Policy: value, Stale: true,
			Note: "unresolved — run the report with remote data to resolve " + value}
	}
	if !force && previous.Policy == value && previous.Resolved != "" {
		if checked, err := time.Parse(time.RFC3339, previous.CheckedAt); err == nil &&
			now.Sub(checked) <= toolchainTTL {
			return previous
		}
	}
	version, source, err := fetchChannelVersion(ctx, client, product, value, now)
	if err != nil {
		pin := previous
		pin.Policy = value
		pin.Stale = true
		if pin.Resolved == "" {
			pin.Note = fmt.Sprintf("unresolved — %v", err)
		}
		return pin
	}
	return PolicyPin{
		Policy:    value,
		Resolved:  version,
		Source:    source,
		CheckedAt: now.Format(time.RFC3339),
	}
}

func trimmedPolicy(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// fetchChannelVersion resolves one channel expression against the product's
// release data source.
func fetchChannelVersion(
	ctx context.Context,
	client *remoteHTTP,
	product toolchainProduct,
	channel string,
	now time.Time,
) (string, string, error) {
	if product.gem != "" {
		if channel != "stable" {
			return "", "", fmt.Errorf("channel %q is not published for %s (only stable)", channel, product.field)
		}
		version, err := fetchGemStable(ctx, client, product)
		return version, "rubygems.org", err
	}
	var cycles []eolCycle
	if err := client.getJSON(ctx, product.eolURL(client), requestAuth{}, &cycles); err != nil {
		return "", "", err
	}
	version, err := pickCycle(cycles, channel, now)
	return version, "endoflife.date", err
}

// eolCycle mirrors the fields used from endoflife.date's per-product JSON.
type eolCycle struct {
	Cycle       string          `json:"cycle"`
	ReleaseDate string          `json:"releaseDate"`
	EOL         json.RawMessage `json:"eol"`
	LTS         json.RawMessage `json:"lts"`
	Latest      string          `json:"latest"`
}

// pickCycle selects the channel's target among already-released cycles:
// stable = newest released cycle, lts = newest cycle whose LTS window opened,
// eol-floor = oldest still-supported cycle.
func pickCycle(cycles []eolCycle, channel string, now time.Time) (string, error) {
	released := []eolCycle{}
	for _, cycle := range cycles {
		if cycle.Latest == "" || futureDate(cycle.ReleaseDate, now) {
			continue
		}
		released = append(released, cycle)
	}
	if len(released) == 0 {
		return "", fmt.Errorf("no released cycle in upstream data")
	}
	// endoflife.date returns newest-first, but do not rely on the order: sort
	// by releaseDate descending so stable/lts pick the newest released cycle.
	slices.SortFunc(released, func(a, b eolCycle) int {
		return strings.Compare(b.ReleaseDate, a.ReleaseDate)
	})
	switch channel {
	case "stable":
		return released[0].Latest, nil
	case "lts":
		for _, cycle := range released {
			if cycle.eolPassed(now) {
				continue
			}
			if lts := cycle.ltsDate(); lts != "" && !futureDate(lts, now) {
				return cycle.Latest, nil
			}
		}
		return "", fmt.Errorf("no active LTS cycle in upstream data")
	case "eol-floor":
		for i := len(released) - 1; i >= 0; i-- {
			if !released[i].eolPassed(now) {
				return released[i].Latest, nil
			}
		}
		return "", fmt.Errorf("no supported cycle in upstream data")
	}
	return "", fmt.Errorf("unknown channel %q", channel)
}

// eolPassed reports whether the cycle's end-of-life marker is a past date or
// the boolean true.
func (c eolCycle) eolPassed(now time.Time) bool {
	raw := strings.Trim(string(c.EOL), `"`)
	switch {
	case len(c.EOL) == 0, raw == "false", raw == "null":
		return false
	case raw == "true":
		return true
	}
	return !futureDate(raw, now)
}

func (c eolCycle) ltsDate() string {
	raw := strings.Trim(string(c.LTS), `"`)
	if len(c.LTS) == 0 || raw == "false" || raw == "null" {
		return ""
	}
	if raw == "true" {
		return "1970-01-01" // boolean LTS flag: always already active
	}
	return raw
}

// futureDate reports whether a date string lies in the future at day
// granularity: YYYY-MM-DD, YYYY-MM (endoflife eol values), or RFC3339.
// Empty or unparseable values count as past.
func futureDate(raw string, now time.Time) bool {
	if raw == "" {
		return false
	}
	for _, layout := range []string{"2006-01-02", "2006-01", time.RFC3339} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return dateAfter(parsed, now)
		}
	}
	return false
}

// fetchGemStable returns the newest non-prerelease version of a gem — the
// versions API answers newest-first.
func fetchGemStable(ctx context.Context, client *remoteHTTP, product toolchainProduct) (string, error) {
	var versions []rubygemsVersion
	if err := client.getJSON(ctx, product.gemURL(client), requestAuth{}, &versions); err != nil {
		return "", err
	}
	for _, version := range versions {
		if !version.Prerelease && version.Number != "" {
			return version.Number, nil
		}
	}
	return "", fmt.Errorf("no stable release in upstream data")
}
