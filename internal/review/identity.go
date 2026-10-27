package review

import (
	"context"
	"errors"
	"fmt"
	"os"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/mcppolicy"
)

var ErrReviewBlocked = errors.New("review blocked")

type IdentityItem struct {
	Path   string `json:"path"`
	ID     string `json:"document_id,omitempty"`
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type IdentityPlan struct {
	Items    []IdentityItem `json:"items"`
	Prepared int            `json:"prepared"`
}

// Identity initialization is a separate reviewed migration. The only mutation
// here is preparing pending proposals; only accepted apply writes UUIDs.
func PlanIdentities(ctx context.Context, c config.Config, db *database.DB, note string, prepare bool) (IdentityPlan, error) {
	result := IdentityPlan{Items: []IdentityItem{}}
	docs, err := db.Documents(ctx)
	if err != nil {
		return result, err
	}
	queue, err := db.ActiveSuggestions(ctx)
	if err != nil {
		return result, err
	}
	active := map[string]bool{}
	for _, s := range queue {
		active[s.DocumentPath] = true
	}
	root, err := os.OpenRoot(c.RepositoryPath)
	if err != nil {
		return result, err
	}
	defer root.Close()
	used, duplicates, err := liveIdentities(ctx, c, db)
	if err != nil {
		return result, err
	}
	for _, doc := range docs {
		if note != "" && doc.Path != note {
			continue
		}
		item := IdentityItem{Path: doc.Path}
		raw, err := readCurrentSource(root, doc.Path, c.MaxDocumentBytes)
		if err != nil {
			item.State, item.Reason = "unavailable", err.Error()
		} else {
			fields, _, parseErr := frontmatter.Parse(raw)
			item.ID, err = frontmatter.DocumentID(fields)
			switch {
			case parseErr != nil:
				item.State, item.Reason = "invalid", parseErr.Error()
			case err != nil:
				item.State, item.Reason = "invalid", err.Error()
			case duplicates[item.ID] && item.ID != "":
				item.State, item.Reason = "duplicate", "duplicate UUID; resolve manually before identity-based repair"
			case item.ID != "":
				item.State = "initialized"
			case active[doc.Path]:
				item.State, item.Reason = "blocked", "finish or retire this note's existing review proposals first"
			default:
				hash, hashErr := hashSource(c.RepositoryPath, doc.Path)
				policyErr := mcppolicy.AssertMutationAllowed(root, doc.Path)
				switch {
				case hashErr != nil:
					item.State, item.Reason = "unavailable", hashErr.Error()
				case hash != doc.SHA256:
					item.State, item.Reason = "blocked", "scan the changed note before initializing identity"
				case policyErr != nil:
					item.State, item.Reason = "blocked", policyErr.Error()
				default:
					item.State = "eligible"
					if prepare {
						id := frontmatter.NewDocumentID()
						for used[id] != "" {
							id = frontmatter.NewDocumentID()
						}
						proposed, err := frontmatter.AddDocumentID(raw, id)
						if err == nil {
							err = db.SaveIdentityProposal(ctx, doc.Path, hash, proposed)
						}
						if err != nil {
							return result, fmt.Errorf("prepared %d identity proposals before stopping (no files changed): %w", result.Prepared, err)
						}
						used[id] = doc.Path
						item.State, item.ID = "prepared", id
						result.Prepared++
					}
				}
			}
		}
		result.Items = append(result.Items, item)
	}
	if note != "" && len(result.Items) == 0 {
		return result, fmt.Errorf("note is not in the scanned corpus: %s", note)
	}
	return result, nil
}

func liveIdentities(ctx context.Context, c config.Config, db *database.DB) (map[string]string, map[string]bool, error) {
	docs, err := db.Documents(ctx)
	if err != nil {
		return nil, nil, err
	}
	root, err := os.OpenRoot(c.RepositoryPath)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	used, duplicates := map[string]string{}, map[string]bool{}
	for _, doc := range docs {
		raw, err := readCurrentSource(root, doc.Path, c.MaxDocumentBytes)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, nil, fmt.Errorf("%w: cannot inventory identity for %s: %v", ErrValidationUnavailable, doc.Path, err)
		}
		fields, _, err := frontmatter.Parse(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: cannot parse identity for %s: %v", ErrValidationUnavailable, doc.Path, err)
		}
		id, err := frontmatter.DocumentID(fields)
		if err != nil || id == "" {
			continue
		}
		if previous := used[id]; previous != "" && previous != doc.Path {
			duplicates[id] = true
		}
		used[id] = doc.Path
	}
	return used, duplicates, nil
}

func validateIdentityReview(ctx context.Context, c config.Config, db *database.DB, s database.Suggestion) error {
	queue, err := db.ActiveSuggestions(ctx)
	if err != nil {
		return err
	}
	for _, other := range queue {
		if other.DocumentPath == s.DocumentPath && other.ID != s.ID {
			return fmt.Errorf("%w: identity initialization waits until other proposals for this note are handled", ErrReviewBlocked)
		}
	}
	fields, _, err := frontmatter.Parse(s.ProposedContent)
	if err != nil {
		return err
	}
	id, err := frontmatter.DocumentID(fields)
	if err != nil || id == "" {
		return fmt.Errorf("identity proposal has no valid UUID")
	}
	used, duplicates, err := liveIdentities(ctx, c, db)
	if err != nil {
		return err
	}
	if duplicates[id] || (used[id] != "" && used[id] != s.DocumentPath) {
		return fmt.Errorf("%w: document UUID is already used by another note", ErrReviewBlocked)
	}
	return nil
}
