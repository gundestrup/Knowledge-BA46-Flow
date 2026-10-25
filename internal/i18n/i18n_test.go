package i18n

import "testing"

func TestNegotiate(t *testing.T) {
	cases := map[string]string{
		"":                    "en",
		"en":                  "en",
		"en-US,en;q=0.9":      "en",
		"da-DK,da;q=0.9":      "da",
		"da_DK.UTF-8":         "da",
		"da:en":               "da",
		"sv-SE,sv;q=0.9":      "en", // unsupported locales fall back to English
		"not a header at all": "en",
	}
	for in, want := range cases {
		if got := Negotiate(in); got != want {
			t.Errorf("Negotiate(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestT(t *testing.T) {
	SetDefault("en")
	if got := T("cli.scan.short"); got != "Scan the repository and sync SQLite (no Ollama calls)" {
		t.Errorf("T(cli.scan.short) = %q", got)
	}
	if got := T("cli.decide.short", map[string]any{"Action": "accept"}); got != "accept a single pending suggestion" {
		t.Errorf("T(cli.decide.short) = %q", got)
	}
	if got := T("ui.landing.cycle_days", map[string]any{"Count": 1}); got != "1 day" {
		t.Errorf("T(cycle_days,1) = %q", got)
	}
	if got := T("ui.landing.cycle_days", map[string]any{"Count": 4}); got != "4 days" {
		t.Errorf("T(cycle_days,4) = %q", got)
	}
	if got := T("missing.key"); got != "missing.key" {
		t.Errorf("missing id should render as itself, got %q", got)
	}

	// Danish catalog renders its own strings, with English as the merged base.
	SetDefault("da-DK")
	defer SetDefault("")
	if got := T("ui.landing.cycle_days", map[string]any{"Count": 1}); got != "1 dag" {
		t.Errorf("da T(cycle_days,1) = %q", got)
	}
	if got := T("ui.landing.cycle_days", map[string]any{"Count": 4}); got != "4 dage" {
		t.Errorf("da T(cycle_days,4) = %q", got)
	}
}

func TestEnvLocale(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if got := EnvLocale(get); got != "" {
		t.Errorf("empty env: got %q", got)
	}
	env["LANG"] = "C"
	env["LC_ALL"] = "C"
	if got := EnvLocale(get); got != "" {
		t.Errorf("C locale should be treated as unset, got %q", got)
	}
	env["LANG"] = "da_DK.UTF-8"
	if got := EnvLocale(get); got != "da_DK.UTF-8" {
		t.Errorf("LANG fallback: got %q", got)
	}
	env["KBM_LANG"] = "de-DE"
	if got := EnvLocale(get); got != "de-DE" {
		t.Errorf("KBM_LANG should win, got %q", got)
	}
}

func TestMessages(t *testing.T) {
	tag, msgs, err := Messages("da-DK,da;q=0.9,en;q=0.8")
	if err != nil {
		t.Fatal(err)
	}
	if tag != "da" {
		t.Fatalf("tag = %q, want da", tag)
	}
	if msgs["ui.kb.hero_title"] == nil {
		t.Error("catalog should expose ui.kb.* keys")
	}
	if msgs["cli.scan.short"] == nil {
		t.Error("catalog should expose cli.* keys")
	}
	if _, ok := msgs["ui.landing.cycle_days"].(map[string]any); !ok {
		t.Error("cycle_days should stay a plural map for the UI's t()")
	}
}
