package frontmatter

import (
	"strings"
	"testing"
)

func TestCanonicalExcludesDocumentID(t *testing.T) {
	base := "---\ntitle: T\ntags: [a]\n---\n# Body\n"
	withID, err := AddDocumentID(base, "2f0a3d3b-4c7d-4e5f-9a8b-7c6d5e4f3a2b")
	if err != nil {
		t.Fatal(err)
	}
	if Canonical(withID) != Canonical(base) {
		t.Fatalf("document_id must be hash-neutral:\n%q\nvs\n%q", Canonical(withID), Canonical(base))
	}
	if strings.Contains(Canonical(withID), "document_id") {
		t.Fatalf("canonical kept identity field: %q", Canonical(withID))
	}
}

// An indented frontmatter mapping (" title:") must get the ID line at the
// same indent — a column-0 insert would produce mixed-indent YAML that no
// parser (or Canonical's own excise) can read.
func TestAddDocumentIDIndentedFrontmatter(t *testing.T) {
	id := "2f0a3d3b-4c7d-4e5f-9a8b-7c6d5e4f3a2b"
	input := "---\n tags:\n    - a\n title: T\n---\n# Body\n"
	out, err := AddDocumentID(input, id)
	if err != nil {
		t.Fatal(err)
	}
	fields, _, err := Parse(out)
	if err != nil {
		t.Fatalf("output unparseable: %v", err)
	}
	if got, _ := DocumentID(fields); got != id {
		t.Fatalf("id=%q", got)
	}
	if !strings.HasPrefix(out, "---\n document_id:") {
		t.Fatalf("id not inserted at the mapping indent:\n%s", out)
	}
	if Canonical(out) != Canonical(input) {
		t.Fatalf("document_id must stay hash-neutral on indented files")
	}
}

func TestAddDocumentIDInsertsFirstFrontmatterLine(t *testing.T) {
	id := "2f0a3d3b-4c7d-4e5f-9a8b-7c6d5e4f3a2b"
	for _, raw := range []string{
		"# No frontmatter\n",
		"---\ntitle: T\n---\n# Body\n",
	} {
		out, err := AddDocumentID(raw, id)
		if err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
		fields, body, err := Parse(out)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DocumentID(fields)
		if err != nil || got != id {
			t.Fatalf("id=%q err=%v", got, err)
		}
		_, oldBody, _ := Parse(raw)
		if body != oldBody {
			t.Fatalf("body changed: %q vs %q", body, oldBody)
		}
		if _, err := AddDocumentID(out, NewDocumentID()); err == nil {
			t.Fatal("duplicate document_id was accepted")
		}
	}
	if _, err := AddDocumentID("# A\n", "not-a-uuid"); err == nil {
		t.Fatal("invalid UUID was accepted")
	}
}

func TestPreserveIdentityInjectsLiveID(t *testing.T) {
	id := "2f0a3d3b-4c7d-4e5f-9a8b-7c6d5e4f3a2b"
	original := "---\ndocument_id: \"" + id + "\"\ntitle: T\n---\n# Body\n"
	// A model that dropped the whole frontmatter block still keeps the ID.
	out, err := PreserveIdentity(original, "# Body\n\nMore\n")
	if err != nil {
		t.Fatal(err)
	}
	fields, _, err := Parse(out)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DocumentID(fields)
	if err != nil || got != id {
		t.Fatalf("live ID not preserved: %q %v", got, err)
	}
	// Already-present identical ID is a no-op.
	same, err := PreserveIdentity(original, original)
	if err != nil || same != original {
		t.Fatalf("identical content changed: %v", err)
	}
}

func TestPreserveIdentityRejectsConflictingOrMalformedID(t *testing.T) {
	original := "---\ndocument_id: \"2f0a3d3b-4c7d-4e5f-9a8b-7c6d5e4f3a2b\"\n---\n# Body\n"
	conflict := "---\ndocument_id: \"9a8b7c6d-5e4f-3a2b-1c0d-9e8f7a6b5c4d\"\n---\n# Body\n"
	if _, err := PreserveIdentity(original, conflict); err == nil {
		t.Fatal("conflicting document_id was not rejected")
	}
	malformed := "---\ndocument_id: not-a-uuid\n---\n# Body\n"
	if _, err := PreserveIdentity(original, malformed); err == nil {
		t.Fatal("malformed document_id was not rejected")
	}
}

func TestPreserveIdentityLeavesAdditionForValidators(t *testing.T) {
	// Original has no ID; a proposal adding one is left untouched — the
	// origin validators (ValidateIdentity vs protected-add in ValidateRewrite)
	// decide whether the insertion is legitimate.
	proposed := "---\ndocument_id: \"2f0a3d3b-4c7d-4e5f-9a8b-7c6d5e4f3a2b\"\n---\n# Body\n"
	out, err := PreserveIdentity("# Body\n", proposed)
	if err != nil || out != proposed {
		t.Fatalf("identity addition was altered: %v", err)
	}
}

func TestDocumentIDFieldValidation(t *testing.T) {
	id := "2F0A3D3B-4C7D-4E5F-9A8B-7C6D5E4F3A2B"
	got, err := DocumentID(map[string]any{"document_id": id})
	if err != nil || got != strings.ToLower(id) {
		t.Fatalf("id=%q err=%v", got, err)
	}
	if got, err := DocumentID(map[string]any{}); err != nil || got != "" {
		t.Fatalf("absent id=%q err=%v", got, err)
	}
	for _, bad := range []any{"not-a-uuid", 42, true} {
		if _, err := DocumentID(map[string]any{"document_id": bad}); err == nil {
			t.Fatalf("malformed %v accepted", bad)
		}
	}
}

func TestValidateIdentityExactInsertion(t *testing.T) {
	original := "---\ntitle: T\n---\n# Body\n"
	proposed, err := AddDocumentID(original, NewDocumentID())
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateIdentity(original, proposed); err != nil {
		t.Fatalf("exact UUID insertion rejected: %v", err)
	}
	if err := ValidateIdentity(original, proposed+"\nExtra\n"); err == nil {
		t.Fatal("identity proposal allowed a body change")
	}
	fields, _, _ := Parse(proposed)
	fields["title"] = "Other"
	other, err := Render(fields, "# Body\n")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateIdentity(original, other); err == nil {
		t.Fatal("identity proposal allowed unrelated metadata changes")
	}
	replacement := "---\ndocument_id: \"9a8b7c6d-5e4f-3a2b-1c0d-9e8f7a6b5c4d\"\n---\n# Body\n"
	if err := ValidateIdentity(proposed, replacement); err == nil {
		t.Fatal("identity proposal allowed replacing an existing ID")
	}
}
