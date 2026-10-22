package server

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Drift checks, API side: every op /api/ops accepts and every JSON field the
// request body understands must be documented in docs/API.md — the same
// contract the CLI drift tests enforce for flags and config keys.

func apiDocs(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "API.md"))
	if err != nil {
		t.Fatalf("read API.md: %v", err)
	}
	return string(data)
}

func TestValidOpsDocumented(t *testing.T) {
	doc := apiDocs(t)
	for _, op := range ValidOps() {
		if !strings.Contains(doc, `"`+op+`"`) && !strings.Contains(doc, "`"+op+"`") {
			t.Errorf("API op %q is not documented in docs/API.md", op)
		}
	}
}

func TestOpRequestFieldsDocumented(t *testing.T) {
	doc := apiDocs(t)
	typ := reflect.TypeOf(opRequest{})
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		// Quoted JSON key or backticked prose mention both count as documented.
		if !strings.Contains(doc, `"`+name+`"`) && !strings.Contains(doc, "`"+name) {
			t.Errorf("op request field %q is not documented in docs/API.md", name)
		}
	}
}
