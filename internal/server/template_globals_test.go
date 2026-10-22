package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Alpine expressions evaluate inside component scope only — host globals such
// as JSON, window, or fetch are not reachable and throw "Undefined variable"
// at render time, freezing the affected bindings. Component methods in
// dashboard.js must wrap any global access instead. This lint scans every
// Alpine attribute expression in the embedded templates so a regression fails
// CI rather than the browser console.

// alpineAttr matches attribute expressions: x-*, :binding, and @event forms.
var alpineAttr = regexp.MustCompile(`(?:x-[a-zA-Z_.:-]*|[:@][a-zA-Z0-9_.:-]*)\s*=\s*"([^"]*)"`)

// forbiddenGlobals are host objects/functions Alpine's scope proxy cannot
// resolve — confirmed in the browser console as "Undefined variable" for
// both JSON and Math. Everything non-trivial belongs in a component method.
var forbiddenGlobals = []string{
	// ECMAScript built-ins
	"JSON", "Math", "Date", "Number", "String", "Boolean", "Array",
	"Object", "Symbol", "Map", "Set", "WeakMap", "WeakSet", "Promise",
	"RegExp", "Error", "Intl", "parseInt", "parseFloat", "isNaN",
	"isFinite", "NaN", "Infinity", "encodeURIComponent",
	"decodeURIComponent", "encodeURI", "decodeURI", "btoa", "atob",
	"structuredClone",
	// DOM / browser APIs
	"window", "globalThis", "localStorage", "sessionStorage",
	"indexedDB", "navigator", "location", "history", "crypto", "fetch",
	"XMLHttpRequest", "alert", "confirm", "prompt", "eval", "Function",
	"setTimeout", "setInterval", "clearTimeout", "clearInterval",
	"requestAnimationFrame", "customElements", "import", "require",
	"console", "URL", "URLSearchParams",
}

func TestTemplatesAvoidForbiddenGlobals(t *testing.T) {
	entries, err := os.ReadDir("static")
	if err != nil {
		t.Fatalf("read static dir: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".html") {
			continue
		}
		path := filepath.Join("static", entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, match := range alpineAttr.FindAllStringSubmatch(string(data), -1) {
			expr := match[1]
			for _, global := range forbiddenGlobals {
				if strings.Contains(expr, global) && containsWord(expr, global) {
					t.Errorf("%s: Alpine expression %q references forbidden global %q; move it into a dashboard.js method", path, expr, global)
				}
			}
		}
	}
}

// containsWord reports whether name appears as a whole word in expr, so a
// denylist hit inside a longer identifier (e.g. "history" inside
// "orderHistory") is not flagged.
func containsWord(expr, name string) bool {
	for idx := 0; ; {
		i := strings.Index(expr[idx:], name)
		if i < 0 {
			return false
		}
		start := idx + i
		end := start + len(name)
		before := start == 0 || !isIdentChar(expr[start-1])
		after := end == len(expr) || !isIdentChar(expr[end])
		if before && after {
			return true
		}
		idx = end
	}
}

func isIdentChar(c byte) bool {
	return c == '_' || c == '$' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}
