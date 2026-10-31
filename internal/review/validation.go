package review

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/mcppolicy"
	"knowledge-ba46-flow/internal/paths"
	"knowledge-ba46-flow/internal/proposal"
	"knowledge-ba46-flow/internal/scanner"
	"knowledge-ba46-flow/internal/taxonomy"
)

type contentValidator func(original, proposed string) error

var ErrValidationUnavailable = errors.New("validation is unavailable")

func suggestionValidator(ctx context.Context, c config.Config, db *database.DB, s database.Suggestion) contentValidator {
	return func(original, proposed string) error {
		root, err := os.OpenRoot(c.RepositoryPath)
		if err != nil {
			return err
		}
		defer root.Close()
		var lookupErr error
		retarget := func(path string) (string, bool) {
			if db == nil {
				return "", false
			}
			target, ok, err := scanner.ObservedRetarget(ctx, c, db, path)
			if err != nil {
				lookupErr = err
			}
			return target, ok
		}
		exists := func(path string) bool {
			local, err := paths.CleanRelative(path)
			if err != nil {
				return false
			}
			info, err := root.Stat(local)
			return err == nil && info.Mode().IsRegular()
		}
		switch s.Origin {
		case "identity":
			err = frontmatter.ValidateIdentity(original, proposed)
		case "deterministic":
			err = proposal.ValidateLinkRepair(s.DocumentPath, original, proposed, retarget, exists)
		case "freshness":
			err = proposal.ValidateRewrite(original, proposed)
			if err == nil {
				err = proposal.ValidateTagsUnchanged(original, proposed)
			}
		default:
			err = proposal.ValidateRewrite(original, proposed)
		}
		if err != nil {
			if lookupErr != nil {
				return fmt.Errorf("%w: %v", ErrValidationUnavailable, lookupErr)
			}
			return err
		}
		return proposal.ValidateBodyScope(c.EffectiveWriteScope(), s.DocumentPath, original, proposed, retarget, exists)
	}
}

func validateForReview(ctx context.Context, c config.Config, db *database.DB, s database.Suggestion) error {
	if err := proposal.ValidateAction(c.EffectiveWriteScope(), s.Action); err != nil {
		return err
	}
	if s.Origin == "identity" {
		if s.Action != "expand" {
			return fmt.Errorf("identity initialization must be an expand proposal")
		}
		if err := validateIdentityReview(ctx, c, db, s); err != nil {
			return err
		}
	}
	root, err := os.OpenRoot(c.RepositoryPath)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrValidationUnavailable, err)
	}
	defer root.Close()
	targets := []string{s.DocumentPath}
	if s.TargetPath != "" {
		targets = append(targets, s.TargetPath)
	}
	if err := mcppolicy.AssertMutationAllowed(root, targets...); err != nil {
		return err
	}
	switch s.Action {
	case "move":
		if s.ProposedContent != "" {
			return fmt.Errorf("move must not contain proposed_content")
		}
		if err := proposal.ValidateMove(s.DocumentPath, s.TargetPath); err != nil {
			return err
		}
		return requireAbsent(root, s.TargetPath, "move target already exists")
	case "delete":
		if s.TargetPath != "" || s.ProposedContent != "" {
			return fmt.Errorf("delete must not contain a target or proposed_content")
		}
		return nil
	case "merge":
		for _, path := range []string{s.DocumentPath, s.TargetPath} {
			if _, err := prepareProposedContent(root, path, s.ProposedContent, c, proposal.ValidateRewrite); err != nil {
				return err
			}
		}
		return nil
	case "expand":
		if s.TargetPath != "" {
			return fmt.Errorf("expand must have an empty target_path")
		}
	default:
		return fmt.Errorf("unsupported action %q", s.Action)
	}
	_, err = prepareProposedContent(root, s.DocumentPath, s.ProposedContent, c, suggestionValidator(ctx, c, db, s))
	return err
}

// Revalidate even old accepted rows against current content and live links,
// carrying live operational metadata into the final write payload.
func prepareProposedContent(root *os.Root, path, proposed string, c config.Config, validate contentValidator) (string, error) {
	original, err := readCurrentSource(root, path, c.MaxDocumentBytes)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrValidationUnavailable, err)
	}
	if _, _, err := frontmatter.Parse(original); err != nil {
		return "", fmt.Errorf("%w: invalid current frontmatter: %v", ErrValidationUnavailable, err)
	}
	proposed, err = frontmatter.PreserveManaged(original, proposed)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	proposed, err = frontmatter.PreserveIdentity(original, proposed)
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if err := validate(original, proposed); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	if frontmatter.ValidateIdentity(original, proposed) == nil {
		return proposed, nil // exact UUID insertion cannot introduce unrelated graph/tag changes.
	}
	if frontmatter.Canonical(original) == frontmatter.Canonical(proposed) {
		return "", fmt.Errorf("%s: proposal makes no canonical content change", path)
	}
	vocabulary, err := taxonomy.LoadFromRoot(root)
	if err != nil {
		return "", fmt.Errorf("%w: load tag taxonomy: %v", ErrValidationUnavailable, err)
	}
	if err := vocabulary.ValidateTagChanges(path, original, proposed); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	err = proposal.ValidateLinks(proposed, func(target string) error {
		extensions := c.Extensions
		if len(extensions) == 0 {
			extensions = []string{".md", ".markdown"}
		}
		document := false
		for _, ext := range extensions {
			if strings.EqualFold(filepath.Ext(target), ext) {
				document = true
				break
			}
		}
		if !document {
			return fmt.Errorf("not a configured document extension")
		}
		info, err := root.Stat(target)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%w: %v", ErrValidationUnavailable, err)
			}
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("not a regular repository document")
		}
		return nil
	})
	return proposed, err
}

func readCurrentSource(root *os.Root, path string, maxBytes int64) (string, error) {
	if _, err := paths.CleanRelative(path); err != nil {
		return "", err
	}
	info, err := root.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("source is not a regular file: %s", path)
	}
	if maxBytes <= 0 {
		maxBytes = 200000
	}
	f, err := root.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	content, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return "", err
	}
	if int64(len(content)) > maxBytes {
		return "", fmt.Errorf("document exceeds max_document_bytes (%d)", maxBytes)
	}
	return string(content), nil
}
