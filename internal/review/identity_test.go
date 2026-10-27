package review

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/scanner"
)

func TestIdentityInitializationIsExplicitReviewedAndQueueSafe(t *testing.T) {
	c, db := queueFixture(t)
	c.Extensions = []string{".md"}
	original := "---\ntitle: A\ncustom: keep\n---\n# A\n\nKeep `code`.\n"
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "a.md"), []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	// Simulate a repository scanned before document_id existed: no scan-time
	// assignment ran, so the row has no identity and the file has no UUID.
	hash := sha256.Sum256([]byte(frontmatter.Canonical(original)))
	if _, err := db.UpsertScanned(t.Context(), database.Document{Path: "a.md", SHA256: hex.EncodeToString(hash[:])}, "s1"); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanIdentities(t.Context(), c, db, "", false)
	if err != nil || len(plan.Items) != 1 || plan.Items[0].State != "eligible" || plan.Prepared != 0 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	queue, _ := db.ActiveSuggestions(t.Context())
	if len(queue) != 0 {
		t.Fatal("identity preview created proposals")
	}
	plan, err = PlanIdentities(t.Context(), c, db, "a.md", true)
	if err != nil || plan.Prepared != 1 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	live, _ := os.ReadFile(filepath.Join(c.RepositoryPath, "a.md"))
	if string(live) != original {
		t.Fatal("identity preparation changed the note")
	}
	queue, _ = db.ActiveSuggestions(t.Context())
	if len(queue) != 1 || queue[0].Origin != "identity" {
		t.Fatalf("queue=%+v", queue)
	}
	again, err := PlanIdentities(t.Context(), c, db, "", true)
	if err != nil || again.Prepared != 0 || again.Items[0].State != "blocked" {
		t.Fatalf("duplicate initialization: %+v %v", again, err)
	}
	if err := DecideCurrent(t.Context(), c, db, strconv.FormatInt(queue[0].ID, 10), "accepted", ""); err != nil {
		t.Fatal(err)
	}
	results, err := Apply(t.Context(), c, db)
	if err != nil || len(results) != 1 || !results[0].Applied {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	after, _ := os.ReadFile(filepath.Join(c.RepositoryPath, "a.md"))
	fields, body, err := frontmatter.Parse(string(after))
	_, oldBody, _ := frontmatter.Parse(original)
	id, idErr := frontmatter.DocumentID(fields)
	if err != nil || idErr != nil || id == "" || body != oldBody || fields["custom"] != "keep" {
		t.Fatalf("identity apply changed unrelated content: %v %v", err, idErr)
	}
}

func TestIdentityValidatorRejectsBodyOrOtherMetadataChanges(t *testing.T) {
	original := "# A\n"
	proposed, err := frontmatter.AddDocumentID(original, frontmatter.NewDocumentID())
	if err != nil {
		t.Fatal(err)
	}
	if err := frontmatter.ValidateIdentity(original, proposed+"\nExtra\n"); err == nil {
		t.Fatal("identity operation granted arbitrary body changes")
	}
	if err := frontmatter.ValidateIdentity(original, proposed); err != nil {
		t.Fatal(err)
	}
}

func TestIDRenamePlusEditRepairsIncomingLinksWithFreshSource(t *testing.T) {
	c, db := queueFixture(t)
	c.Extensions = []string{".md"}
	id := frontmatter.NewDocumentID()
	target := "---\ndocument_id: \"" + id + "\"\n---\n# Target\n"
	source := "# Source\n\n[Target](../old.md?mode=read#part)\n\n`[example](../old.md)`\n"
	if err := os.MkdirAll(filepath.Join(c.RepositoryPath, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	for path, raw := range map[string]string{"old.md": target, "sub/source.md": source} {
		if err := os.WriteFile(filepath.Join(c.RepositoryPath, filepath.FromSlash(path)), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := scanner.Scan(t.Context(), c, db); err != nil {
		t.Fatal(err)
	}
	docs, _ := db.Documents(t.Context())
	for _, doc := range docs {
		if doc.Path == "old.md" {
			if err := db.SaveAnalysis(t.Context(), doc, "fake", "", "", nil, []database.Suggestion{{
				Action: "expand", ProposedContent: target + "\nOld snapshot addition\n",
			}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Simulate an external rename and edit, never a KBM-flow move operation.
	if err := os.Rename(filepath.Join(c.RepositoryPath, "old.md"), filepath.Join(c.RepositoryPath, "new.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "new.md"), []byte(target+"\nEdited target body\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := scanner.Scan(t.Context(), c, db)
	if err != nil || result.Changed != 1 {
		t.Fatalf("scan=%+v err=%v", result, err)
	}
	old, err := db.Suggestion(t.Context(), 1)
	if err != nil || old.DocumentPath != "new.md" || old.Status != "superseded" || old.Error == "" {
		t.Fatalf("identity must not override freshness: %+v %v", old, err)
	}
	inbound, err := db.InboundLinks(t.Context(), "new.md")
	if err != nil || len(inbound) != 1 || inbound[0] != "sub/source.md" {
		t.Fatalf("reverse index lost incoming identity: %v %v", inbound, err)
	}
	live, err := os.ReadFile(filepath.Join(c.RepositoryPath, "sub/source.md"))
	if err != nil {
		t.Fatal(err)
	}
	proposed := strings.Replace(string(live), "../old.md?mode=read#part", "../new.md?mode=read#part", 1)
	docs, _ = db.Documents(t.Context())
	for _, doc := range docs {
		if doc.Path == "sub/source.md" {
			if err := db.SaveLinkFix(t.Context(), doc, []database.Suggestion{{
				Action: "expand", Origin: "deterministic", ProposedContent: proposed,
			}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	queue, _ := db.ActiveSuggestions(t.Context())
	if len(queue) != 1 {
		t.Fatalf("queue=%+v", queue)
	}
	s := queue[0]
	if err := DecideCurrent(t.Context(), c, db, strconv.FormatInt(s.ID, 10), "accepted", ""); err != nil {
		t.Fatal(err)
	}
	results, err := Apply(t.Context(), c, db)
	if err != nil || len(results) != 1 || !results[0].Applied {
		t.Fatalf("results=%+v err=%v", results, err)
	}
	actual, _ := os.ReadFile(filepath.Join(c.RepositoryPath, "sub/source.md"))
	if string(actual) != proposed {
		t.Fatalf("query/fragment/code were not preserved: %q", actual)
	}
	// A later UUID replacement without another scan cannot inherit the move.
	replaced := strings.Replace(target, id, frontmatter.NewDocumentID(), 1)
	if err := os.WriteFile(filepath.Join(c.RepositoryPath, "new.md"), []byte(replaced), 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := scanner.ObservedRetarget(t.Context(), c, db, "old.md"); err != nil || ok {
		t.Fatalf("live identity replacement was trusted: ok=%v err=%v", ok, err)
	}
}
