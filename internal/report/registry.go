package report

import (
	"fmt"
	"os"

	"knowledge-ba46-flow/internal/config"

	"go.yaml.in/yaml/v3"
)

type repositoryEntry struct {
	URL         string              `yaml:"url"`
	Path        string              `yaml:"path"`
	SyncedAt    string              `yaml:"synced_at"`
	RepoCommit  string              `yaml:"repo_commit"`
	KBCommit    string              `yaml:"kb_commit"`
	SemgrepRepo string              `yaml:"semgrep_repo"`
	Tags        map[string][]string `yaml:"tags"`
}

type registryFile struct {
	Repos map[string]repositoryEntry `yaml:"repos"`
}

type policyFile struct {
	Versions struct {
		Ruby     string `yaml:"ruby"`
		Node     string `yaml:"node"`
		Python   string `yaml:"python"`
		Go       string `yaml:"go"`
		Php      string `yaml:"php"`
		Bundler  string `yaml:"bundler"`
		Rubygems string `yaml:"rubygems"`
		// Custom covers products without a built-in field: the key is the
		// product name (endoflife.date slug for channels); detection is the
		// generic .tool-versions / .<name>-version convention.
		Custom map[string]string `yaml:"custom"`
	} `yaml:"versions"`
	Checks struct {
		RequireRubyVersionFile bool                           `yaml:"require_ruby_version_file"`
		RequireNodeVersionFile bool                           `yaml:"require_node_version_file"`
		TagReleaseExceptions   map[string]TagReleaseException `yaml:"tag_release_exceptions"`
		// MinCoverage flags repositories whose Codecov coverage falls below
		// this percentage. Unset means no coverage threshold is enforced.
		MinCoverage *float64 `yaml:"min_coverage"`
	} `yaml:"checks"`
	Gems struct {
		Slots     map[string]GemSlot      `yaml:"slots"`
		Watch     map[string]string       `yaml:"watch"`
		DriftOK   map[string]string       `yaml:"drift_ok"`
		DormantOK map[string]DormantGemOK `yaml:"dormant_ok"`
		Hold      map[string]string       `yaml:"hold"`
	} `yaml:"gems"`
	NPM struct {
		DriftOK map[string]string `yaml:"drift_ok"`
		Watch   map[string]string `yaml:"watch"`
		Hold    map[string]string `yaml:"hold"`
	} `yaml:"npm"`
}

type GemSlot struct {
	Prefer string   `yaml:"prefer"`
	OK     []string `yaml:"ok"`
}

type DormantGemOK struct {
	Reviewed    string `yaml:"reviewed"`
	ReviewAfter int    `yaml:"review_after"`
	Reason      string `yaml:"reason"`
}

func loadSources(cfg config.ReportConfig) (map[string]repositoryEntry, policyFile, error) {
	if cfg.RegistryPath == "" {
		return nil, policyFile{}, fmt.Errorf("report.registry_path is not configured")
	}
	registryData, err := os.ReadFile(cfg.RegistryPath)
	if err != nil {
		return nil, policyFile{}, fmt.Errorf("read report registry: %w", err)
	}
	var registry registryFile
	if err := yaml.Unmarshal(registryData, &registry); err != nil {
		return nil, policyFile{}, fmt.Errorf("parse report registry: %w", err)
	}
	if registry.Repos == nil {
		return nil, policyFile{}, fmt.Errorf("report registry must contain a repos mapping")
	}

	var policy policyFile
	if cfg.PolicyPath != "" {
		policyData, err := os.ReadFile(cfg.PolicyPath)
		if err != nil {
			return nil, policyFile{}, fmt.Errorf("read report policy: %w", err)
		}
		if err := yaml.Unmarshal(policyData, &policy); err != nil {
			return nil, policyFile{}, fmt.Errorf("parse report policy: %w", err)
		}
	}
	return registry.Repos, policy, nil
}

// ValidateRepository confirms that a requested single-repository refresh
// names an entry in the configured registry.
func ValidateRepository(cfg config.ReportConfig, name string) error {
	registry, _, err := loadSources(cfg)
	if err != nil {
		return err
	}
	if _, ok := registry[name]; !ok {
		return fmt.Errorf("unknown repository %q", name)
	}
	return nil
}
