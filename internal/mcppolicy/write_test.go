package mcppolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssertMutationAllowed(t *testing.T) {
	tests := []struct {
		name    string
		policy  string
		wantErr string
	}{
		{name: "missing policy allows"},
		{name: "empty object allows", policy: `{}`},
		{name: "null mutation value allows", policy: `{"mutations": null }`},
		{name: "explicit allow", policy: `{"mutations":"allow"}`},
		{name: "deny", policy: `{"mutations":"deny"}`, wantErr: "denies mutations"},
		{name: "malformed json fails closed", policy: `{`, wantErr: "writes are denied"},
		{name: "unknown key fails closed", policy: `{"other":true}`, wantErr: "writes are denied"},
		{name: "invalid value fails closed", policy: `{"mutations":"sometimes"}`, wantErr: "writes are denied"},
		{name: "wrong value type fails closed", policy: `{"mutations":true}`, wantErr: "writes are denied"},
		{name: "non-object fails closed", policy: `[]`, wantErr: "writes are denied"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := t.TempDir()
			if tt.policy != "" {
				if err := os.WriteFile(filepath.Join(repo, writePolicyFilename), []byte(tt.policy), 0600); err != nil {
					t.Fatal(err)
				}
			}
			root, err := os.OpenRoot(repo)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			err = AssertMutationAllowed(root, "note.md")
			if tt.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("error=%v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestAssertMutationAllowedChecksEveryTouchedShelf(t *testing.T) {
	repo := t.TempDir()
	for shelf, value := range map[string]string{
		"alpha": `{"mutations":"allow"}`,
		"beta":  `{"mutations":"deny"}`,
	} {
		dir := filepath.Join(repo, shelf)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, writePolicyFilename), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	err = AssertMutationAllowed(root, "alpha/source.md", "beta/target.md")
	if err == nil || !strings.Contains(err.Error(), "beta/.kb-policy.json") {
		t.Fatalf("error=%v, want beta shelf denial", err)
	}
}

func TestAssertMutationAllowedProtectsPolicyFiles(t *testing.T) {
	repo := t.TempDir()
	root, err := os.OpenRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for _, path := range []string{".kb-policy.json", "shelf/.kb-policy.json"} {
		if err := AssertMutationAllowed(root, path); err == nil || !strings.Contains(err.Error(), "managed policy") {
			t.Fatalf("path=%q error=%v, want managed-policy rejection", path, err)
		}
	}
}

func TestAssertMutationAllowedRejectsPolicySymlinkAndTraversal(t *testing.T) {
	repo := t.TempDir()
	outside := filepath.Join(t.TempDir(), "policy.json")
	if err := os.WriteFile(outside, []byte(`{"mutations":"allow"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, writePolicyFilename)); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	root, err := os.OpenRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := AssertMutationAllowed(root, "note.md"); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink policy error=%v", err)
	}
	if err := AssertMutationAllowed(root, "../outside.md"); err == nil {
		t.Fatal("expected traversal rejection")
	}
}

func TestAssertMutationAllowedRejectsOversizedPolicy(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, writePolicyFilename), []byte(strings.Repeat(" ", maxWritePolicyBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := AssertMutationAllowed(root, "note.md"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error=%v, want oversized policy rejection", err)
	}
}
