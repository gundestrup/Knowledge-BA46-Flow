package config

import (
	"strings"
	"testing"
	"time"
)

func TestEnrichmentLimits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout int
		tokens  int
	}{
		{"defaults", 0, 0},
		{"overrides", 45, 256},
		{"negative timeout", -1, 512},
		{"negative tokens", 120, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := requiredConfig()
			values["request_timeout_seconds"] = 600
			values["enrichment_timeout_seconds"] = tc.timeout
			values["enrichment_max_tokens"] = tc.tokens
			c, err := Load(writeConfig(t, values))
			if tc.timeout < 0 || tc.tokens < 0 {
				if err == nil || !strings.Contains(err.Error(), "enrichment_") {
					t.Fatalf("expected invalid enrichment limits, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantTimeout, wantTokens := tc.timeout, tc.tokens
			if wantTimeout == 0 {
				wantTimeout = 120
			}
			if wantTokens == 0 {
				wantTokens = 512
			}
			if c.EnrichmentTimeout() != time.Duration(wantTimeout)*time.Second || c.EnrichmentOutputTokens() != wantTokens || c.RequestTimeoutSeconds != 600 {
				t.Fatalf("limits=%+v", c)
			}
		})
	}
	if (Config{}).EnrichmentTimeout() != 120*time.Second || (Config{}).EnrichmentOutputTokens() != 512 {
		t.Fatal("programmatic configurations must retain bounded defaults")
	}
}
