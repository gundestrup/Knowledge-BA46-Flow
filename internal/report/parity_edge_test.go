package report

import "testing"

func TestParityEdgeCases(t *testing.T) {
	cases := []struct {
		name string
		got  bool
		want bool
	}{
		{"hyphen range hit", npmRangeSatisfies("1.0.0 - 2.0.0", "1.5.0"), true},
		{"hyphen range miss", npmRangeSatisfies("1.0.0 - 2.0.0", "3.0.0"), false},
		{"caret", npmRangeSatisfies("^10.1.0", "10.9.9"), true},
		{"caret major-blocked", npmRangeSatisfies("^10.1.0", "11.0.0"), false},
		{"caret zero-major", npmRangeSatisfies("^0.2.0", "0.3.0"), false},
		{"tilde", npmRangeSatisfies("~1.2.0", "1.2.9"), true},
		{"tilde minor-blocked", npmRangeSatisfies("~1.2.0", "1.3.0"), false},
		{"or-alternative", npmRangeSatisfies("^1.0.0 || >=3.0.0", "3.1.0"), true},
		{"wildcard-req lenient", npmRangeSatisfies("workspace:*", "1.0.0"), true},
		{"junk-req lenient", npmRangeSatisfies("file:../x", "1.0.0"), true},
		{"partial candidate", npmRangeSatisfies(">=18.0.0", "22"), true},
		{"partial below", npmRangeSatisfies(">=18.0.0", "16"), false},
		{"x-range req", npmRangeSatisfies("10.x", "10.99.0"), true},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %v want %v", c.name, c.got, c.want)
		}
	}
	gemCases := []struct {
		name string
		got  bool
		want bool
	}{
		{"gem pessimistic", gemRequirementSatisfied("~> 3.5.0", mustGem(t, "3.5.9")), true},
		{"gem pessimistic bound", gemRequirementSatisfied("~> 3.5.0", mustGem(t, "3.6.0")), false},
		{"gem pessimistic major", gemRequirementSatisfied("~> 3", mustGem(t, "4.0.0")), false},
		{"gem not-equal", gemRequirementSatisfied("!= 2.0.0", mustGem(t, "2.0.1")), true},
		{"gem multi-clause", gemRequirementSatisfied(">= 1.0, < 2.0", mustGem(t, "1.5.0")), true},
		{"gem junk lenient", gemRequirementSatisfied("not-a-version", mustGem(t, "1.0")), true},
		{"gem prerelease order", compareGemVersions(mustGem(t, "1.0.0.pre"), mustGem(t, "1.0.0")) < 0, true},
	}
	for _, c := range gemCases {
		if c.got != c.want {
			t.Errorf("%s: got %v want %v", c.name, c.got, c.want)
		}
	}
}

func mustGem(t *testing.T, raw string) gemVersion {
	t.Helper()
	v, ok := parseGemVersion(raw)
	if !ok {
		t.Fatalf("unparseable gem version %q", raw)
	}
	return v
}
