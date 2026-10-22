package report

const (
	ReportSchemaVersion      = 4
	ReportGemBaselineVersion = 3
)

type PullRequest struct {
	Number int    `json:"n"`
	Title  string `json:"t"`
	URL    string `json:"u,omitempty"`
}

type GitHubInfo struct {
	Topics             *[]string      `json:"topics"`
	Private            *bool          `json:"private"`
	OpenPRs            *int           `json:"open_prs"`
	PRTitles           *[]string      `json:"pr_titles"`
	PRs                *[]PullRequest `json:"prs"`
	CI                 *string        `json:"ci"`
	TagsWithoutRelease *[]string      `json:"tags_without_release"`
	ReleasesWithoutTag *[]string      `json:"releases_without_tag"`
	CodeScanning       *int           `json:"code_scanning"`
	Dependabot         *int           `json:"dependabot"`
	DefaultBranch      *string        `json:"default_branch,omitempty"`
	AuthUnavailable    bool           `json:"auth_unavailable,omitempty"`
	StaleFields        []string       `json:"stale_fields,omitempty"`
}

type ForgejoInfo struct {
	OpenPRs            *int           `json:"open_prs"`
	PRTitles           *[]string      `json:"pr_titles"`
	PRs                *[]PullRequest `json:"prs"`
	ActionRuns         *int           `json:"action_runs"`
	TagsWithoutRelease *[]string      `json:"tags_without_release"`
	ReleasesWithoutTag *[]string      `json:"releases_without_tag"`
	AuthUnavailable    bool           `json:"auth_unavailable,omitempty"`
	StaleFields        []string       `json:"stale_fields,omitempty"`
}

type SemgrepFindings struct {
	Project    *string        `json:"project"`
	Open       *int           `json:"open"`
	Severities map[string]int `json:"severities,omitempty"`
	URL        string         `json:"url,omitempty"`
	Stale      bool           `json:"stale,omitempty"`
}

type GemEdge struct {
	By  string `json:"by"`
	Req string `json:"req,omitempty"`
	Src string `json:"src"`
}

type GemspecDependency struct {
	Req string `json:"req"`
	Dev bool   `json:"dev"`
}

type GemChanges struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
}

type GemLatest struct {
	Version   string `json:"v,omitempty"`
	Released  string `json:"at,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
}

type NPMProject struct {
	Path             string               `json:"path"`
	Name             string               `json:"name,omitempty"`
	Version          string               `json:"version,omitempty"`
	PackageManager   string               `json:"package_manager,omitempty"`
	NodeEngine       string               `json:"node_engine,omitempty"`
	NodeVersion      string               `json:"node_version,omitempty"`
	NodeVersionFile  string               `json:"node_version_file,omitempty"`
	Lockfile         string               `json:"lockfile,omitempty"`
	LockfileVersion  int                  `json:"lockfile_version,omitempty"`
	LockfileMissing  bool                 `json:"lockfile_missing"`
	Direct           map[string]string    `json:"direct"`
	Packages         map[string]string    `json:"packages"`
	PackageVersions  map[string][]string  `json:"package_versions"`
	MultipleVersions []string             `json:"multiple_versions"`
	Edges            map[string][]NPMEdge `json:"edges"`
	Watched          map[string]string    `json:"watched"`
}

type NPMEdge struct {
	By      string `json:"by"`
	Req     string `json:"req,omitempty"`
	Project string `json:"project"`
}

type NPMLatest struct {
	Version   string `json:"version,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
	FailedAt  string `json:"failed_at,omitempty"`
	Available bool   `json:"available,omitempty"`
	Stale     bool   `json:"stale,omitempty"`
}

type NodeRelease struct {
	Version string `json:"version"`
	LTS     string `json:"lts,omitempty"`
}

type NodeReleaseCache struct {
	ByMajor   map[string]NodeRelease `json:"by_major"`
	CheckedAt string                 `json:"checked_at,omitempty"`
	FailedAt  string                 `json:"failed_at,omitempty"`
	Stale     bool                   `json:"stale,omitempty"`
}

type NodeOutdated struct {
	Pinned   string `json:"pinned"`
	Latest   string `json:"latest"`
	LTS      string `json:"lts,omitempty"`
	Severity string `json:"severity"`
	Project  string `json:"project"`
}

type NPMDrift struct {
	Versions map[string][]string `json:"versions"`
	Severity string              `json:"severity"`
}

type AllowedNPMDrift struct {
	Versions map[string][]string `json:"versions"`
	Reason   string              `json:"reason"`
}

type NPMOutdated struct {
	Latest    string              `json:"latest"`
	Versions  map[string][]string `json:"versions"`
	Severity  string              `json:"severity"`
	BlockedBy []string            `json:"blocked_by"`
}

// NPMHold is an outdated package whose lag is documented policy rather than a
// finding: npm.hold records the reason so the report shows "held", not "behind".
type NPMHold struct {
	Latest    string              `json:"latest"`
	Versions  map[string][]string `json:"versions"`
	Reason    string              `json:"reason"`
	BlockedBy []string            `json:"blocked_by,omitempty"`
}

type NPMFleetSummary struct {
	NodeVersionDrift map[string][]string        `json:"node_version_drift"`
	NodeOutdated     map[string]NodeOutdated    `json:"node_outdated"`
	PackageDrift     map[string]NPMDrift        `json:"package_drift"`
	PackageDriftOK   map[string]AllowedNPMDrift `json:"package_drift_ok"`
	PackageOutdated  map[string]NPMOutdated     `json:"package_outdated"`
	PackageHeld      map[string]NPMHold         `json:"package_held"`
}

type GemDrift struct {
	Versions map[string][]string `json:"versions"`
	Severity string              `json:"severity"`
}

type AllowedGemDrift struct {
	Versions map[string][]string `json:"versions"`
	Reason   string              `json:"reason"`
}

type GemOutdated struct {
	Latest    string              `json:"latest"`
	Versions  map[string][]string `json:"versions"`
	Severity  string              `json:"severity"`
	BlockedBy []string            `json:"blocked_by"`
}

// GemHold is an outdated gem whose lag is documented policy rather than a
// finding: gems.hold records the reason so the report shows "held", not
// "behind". It complements the automatic "constrained" severity, which only
// fires when a lockfile dependency edge explains the hold.
type GemHold struct {
	Latest    string              `json:"latest"`
	Versions  map[string][]string `json:"versions"`
	Reason    string              `json:"reason"`
	BlockedBy []string            `json:"blocked_by,omitempty"`
}

type GemStaleness struct {
	Latest    string              `json:"latest"`
	AgeMonths int                 `json:"age_months"`
	Versions  map[string][]string `json:"versions"`
	Severity  string              `json:"severity"`
	Note      string              `json:"note,omitempty"`
}

type FleetSummary struct {
	GemDrift    map[string]GemDrift        `json:"gem_drift"`
	GemDriftOK  map[string]AllowedGemDrift `json:"gem_drift_ok"`
	GemOutdated map[string]GemOutdated     `json:"gem_outdated"`
	GemHeld     map[string]GemHold         `json:"gem_held"`
	GemStale    map[string]GemStaleness    `json:"gem_stale"`
	NPM         NPMFleetSummary            `json:"npm"`
}

type TagReleaseException struct {
	TagsWithoutRelease []string `yaml:"tags_without_release" json:"tags_without_release,omitempty"`
	ReleasesWithoutTag []string `yaml:"releases_without_tag" json:"releases_without_tag,omitempty"`
}

type PolicyExceptions struct {
	TagRelease map[string]TagReleaseException `json:"tag_release"`
}
