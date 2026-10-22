// Command seed records deterministic analyzed state and suggestions in a
// fixture state.db (created by `KBM-flow scan`) so the browser e2e suite can
// exercise review flows without a live model.
//
// Usage:
//
//	go run ./scripts/e2e/seed -db <state.db> -repo <repository root>
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"knowledge-ba46-flow/internal/database"
)

const applyMarker = "Seeded e2e paragraph — apply marker."

func main() {
	dbPath := flag.String("db", "", "path to the fixture state.db")
	repoPath := flag.String("repo", "", "knowledge repository root for reading document bodies")
	flag.Parse()
	if *dbPath == "" || *repoPath == "" {
		fmt.Fprintln(os.Stderr, "usage: seed -db <state.db> -repo <repository root>")
		os.Exit(2)
	}
	if err := run(*dbPath, *repoPath); err != nil {
		fmt.Fprintf(os.Stderr, "seed: %v\n", err)
		os.Exit(1)
	}
}

func run(dbPath, repoPath string) error {
	ctx := context.Background()
	db, err := database.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	docs, err := db.Documents(ctx)
	if err != nil {
		return err
	}
	if len(docs) < 3 {
		return fmt.Errorf("fixture needs at least 3 scanned documents, found %d", len(docs))
	}

	// One suggestion per document, with distinct origins so the origin filter
	// visibly changes the rendered list.
	plans := []struct {
		doc    database.Document
		origin string
		build  func() (database.Suggestion, error)
	}{
		{docs[0], "analysis", func() (database.Suggestion, error) {
			// expand with proposed content = original + an appended marker
			// paragraph passes every apply-time validator (additive body edit,
			// preserved frontmatter), giving the e2e a safe mutation to assert.
			content, err := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(docs[0].Path))) // #nosec G304 -- e2e fixture path from CLI flags
			if err != nil {
				return database.Suggestion{}, err
			}
			proposed := strings.TrimRight(string(content), "\n") + "\n\n" + applyMarker + "\n"
			return database.Suggestion{
				Action:          "expand",
				Reason:          "e2e seeded expansion accepted then applied",
				Confidence:      0.9,
				ProposedContent: proposed,
			}, nil
		}},
		{docs[1], "enrich", func() (database.Suggestion, error) {
			return database.Suggestion{
				Action:     "move",
				TargetPath: "reviewed/" + docs[1].Path,
				Reason:     "e2e seeded move rejected during review",
				Confidence: 0.8,
			}, nil
		}},
		{docs[2], "deterministic", func() (database.Suggestion, error) {
			return database.Suggestion{
				Action:     "move",
				TargetPath: "sorted/" + docs[2].Path,
				Reason:     "e2e seeded deterministic move left pending",
				Confidence: 0.7,
			}, nil
		}},
	}

	for _, plan := range plans {
		suggestion, err := plan.build()
		if err != nil {
			return err
		}
		suggestion.Origin = plan.origin
		if err := db.SaveAnalysis(ctx, plan.doc, "e2e-seed", "E2E fixture doc", "seeded for browser tests", []string{"e2e"}, []database.Suggestion{suggestion}); err != nil {
			return fmt.Errorf("save analysis for %s: %w", plan.doc.Path, err)
		}
		fmt.Printf("seeded %s suggestion on %s\n", suggestion.Action, plan.doc.Path)
	}
	return nil
}
