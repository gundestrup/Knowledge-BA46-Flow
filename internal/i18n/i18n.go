// Package i18n owns user-facing text: embedded YAML catalogs rendered
// through go-i18n for Go-side strings (CLI output, job detail) and a merged
// raw-catalog dump the web UI resolves in its own t(). Adding a locale
// means a new locales/active.<tag>.yaml file and its language.Tag in
// supported — no other code changes.
package i18n

import (
	"embed"
	"errors"
	"io/fs"
	"strings"
	"sync/atomic"

	goi18n "github.com/nicksnyder/go-i18n/v2/i18n"
	"go.yaml.in/yaml/v3"
	"golang.org/x/text/language"
)

// DefaultLocale is the base catalog and the ultimate fallback.
const DefaultLocale = "en"

//go:embed locales
var localeFS embed.FS

var supported = []language.Tag{language.English, language.Danish}

var (
	bundle  *goi18n.Bundle
	matcher language.Matcher
	process atomic.Pointer[Localizer]
)

func init() {
	bundle = goi18n.NewBundle(language.English)
	bundle.RegisterUnmarshalFunc("yaml", yaml.Unmarshal)
	matcher = language.NewMatcher(supported, language.PreferSameScript(true))
	entries, err := localeFS.ReadDir("locales")
	if err != nil {
		panic("i18n: read locales: " + err.Error())
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "active.") || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		if _, err := bundle.LoadMessageFileFS(localeFS, "locales/"+e.Name()); err != nil {
			panic("i18n: load catalog " + e.Name() + ": " + err.Error())
		}
	}
	process.Store(For(""))
}

// Negotiate resolves an Accept-Language header value or a POSIX locale
// ("da_DK.UTF-8", "da:en") to the best supported tag — currently English
// or Danish, English as the ultimate fallback.
func Negotiate(acceptLanguage string) string {
	v := strings.TrimSpace(acceptLanguage)
	if v == "" {
		return DefaultLocale
	}
	if !strings.ContainsAny(v, ",;") {
		v, _, _ = strings.Cut(v, ".")
		v, _, _ = strings.Cut(v, ":")
	}
	v = strings.ReplaceAll(v, "_", "-")
	tags, _, err := language.ParseAcceptLanguage(v)
	if err != nil || len(tags) == 0 {
		return DefaultLocale
	}
	tag, _, _ := matcher.Match(tags...)
	base, _ := tag.Base()
	return base.String()
}

// Supported returns the base tags with a shipped catalog — the UI offers
// these in its locale picker.
func Supported() []string {
	out := make([]string, len(supported))
	for i, t := range supported {
		base, _ := t.Base()
		out[i] = base.String()
	}
	return out
}

// Localizer renders messages for one resolved locale.
type Localizer struct{ inner *goi18n.Localizer }

// For returns a Localizer for the negotiated form of acceptLanguage — one
// per HTTP request for Accept-Language, once at startup for the CLI.
func For(acceptLanguage string) *Localizer {
	return &Localizer{goi18n.NewLocalizer(bundle, Negotiate(acceptLanguage))}
}

// T renders a catalog message. The optional single argument is the template
// data — a map[string]any whose "Count" entry also selects the plural form.
// Unknown ids render as the id itself so a missing entry is visible.
func (l *Localizer) T(id string, data ...any) string {
	cfg := &goi18n.LocalizeConfig{MessageID: id}
	if len(data) > 0 {
		cfg.TemplateData = data[0]
		if m, ok := data[0].(map[string]any); ok {
			if count, ok := m["Count"]; ok {
				cfg.PluralCount = count
			}
		}
	}
	s, err := l.inner.Localize(cfg)
	if err != nil {
		return id
	}
	return s
}

// SetDefault binds the process-wide localizer used by T — call once at
// startup with the resolved environment locale.
func SetDefault(acceptLanguage string) {
	process.Store(For(acceptLanguage))
}

// T renders id for the process default — CLI and server-side job output.
// Use For(acceptLanguage).T where a request locale applies instead.
func T(id string, data ...any) string {
	return process.Load().T(id, data...)
}

// EnvLocale reads the environment for a locale choice: KBM_LANG wins as the
// explicit opt-in, then the usual GNU/POSIX variables. Empty means default.
func EnvLocale(getenv func(string) string) string {
	for _, key := range []string{"KBM_LANG", "LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := strings.TrimSpace(getenv(key)); v != "" && v != "C" && v != "POSIX" {
			return v
		}
	}
	return ""
}

// Messages returns the flattened message catalog for the negotiated tag —
// the locale's own file merged over the English base so partial
// translations still render — for the UI's t() dictionary.
func Messages(acceptLanguage string) (string, map[string]any, error) {
	tag := Negotiate(acceptLanguage)
	out, err := loadCatalog("active." + DefaultLocale + ".yaml")
	if err != nil {
		return "", nil, err
	}
	if tag != DefaultLocale {
		over, err := loadCatalog("active." + tag + ".yaml")
		if err != nil {
			return "", nil, err
		}
		for k, v := range over {
			out[k] = v
		}
	}
	return tag, out, nil
}

func loadCatalog(name string) (map[string]any, error) {
	raw, err := localeFS.ReadFile("locales/" + name)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}
