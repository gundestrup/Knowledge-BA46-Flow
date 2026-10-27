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
		item, prepared, err := planIdentityItem(ctx, c, db, root, doc, active, used, duplicates, prepare)
		if err != nil {
			return result, fmt.Errorf("prepared %d identity proposals before stopping (no files changed): %w", result.Prepared, err)
		}
		result.Items = append(result.Items, item)
		if prepared {
			result.Prepared++
		}
	}
	if note != "" && len(result.Items) == 0 {
		return result, fmt.Errorf("note is not in the scanned corpus: %s", note)
	}
	return result, nil
}

func planIdentityItem(
	ctx context.Context,
	c config.Config,
	db *database.DB,
	root *os.Root,
	doc database.Document,
	active map[string]bool,
	used map[string]string,
	duplicates map[string]bool,
	prepare bool,
) (IdentityItem, bool, error) {
	item := IdentityItem{Path: doc.Path}
	raw, err := readCurrentSource(root, doc.Path, c.MaxDocumentBytes)
	if err != nil {
		item.State, item.Reason = "unavailable", err.Error()
		return item, false, nil
	}
	fields, _, parseErr := frontmatter.Parse(raw)
	if parseErr != nil {
		item.State, item.Reason = "invalid", parseErr.Error()
		return item, false, nil
	}
	item.ID, err = frontmatter.DocumentID(fields)
	if err != nil {
		item.State, item.Reason = "invalid", err.Error()
		return item, false, nil
	}
	if item.ID != "" && duplicates[item.ID] {
		item.State, item.Reason = "duplicate", "duplicate UUID; resolve manually before identity-based repair"
		return item, false, nil
	}
	if item.ID != "" {
		item.State = "initialized"
		return item, false, nil
	}
	if active[doc.Path] {
		item.State, item.Reason = "blocked", "finish or retire this note's existing review proposals first"
		return item, false, nil
	}
	hash, err := hashSource(c.RepositoryPath, doc.Path)
	if err != nil {
		item.State, item.Reason = "unavailable", err.Error()
		return item, false, nil
	}
	if hash != doc.SHA256 {
		item.State, item.Reason = "blocked", "scan the changed note before initializing identity"
		return item, false, nil
	}
	if err := mcppolicy.AssertMutationAllowed(root, doc.Path); err != nil {
		item.State, item.Reason = "blocked", err.Error()
		return item, false, nil
	}
	item.State = "eligible"
	if !prepare {
		return item, false, nil
	}
	id := frontmatter.NewDocumentID()
	for used[id] != "" {
		id = frontmatter.NewDocumentID()
	}
	proposed, err := frontmatter.AddDocumentID(raw, id)
	if err == nil {
		proposed, err = frontmatter.NormalizeOrder(proposed)
	}
	if err == nil {
		err = db.SaveIdentityProposal(ctx, doc.Path, hash, proposed)
	}
	if err != nil {
		return item, false, err
	}
	used[id] = doc.Path
	item.State, item.ID = "prepared", id
	return item, true, nil
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
