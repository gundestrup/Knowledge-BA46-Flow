package proposal

import (
	"errors"
	"fmt"
	"reflect"
	"slices"

	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/links"
)

// ErrWriteScope is a permission block, not evidence of an obsolete proposal.
var ErrWriteScope = errors.New("blocked by write_scope")

func ValidateAction(scope, action string) error {
	switch scope {
	case "metadata-links", "content":
		if action != "expand" && action != "none" {
			return fmt.Errorf("%w=%s: %s requires explicit write_scope=all", ErrWriteScope, scope, action)
		}
	case "all":
	default:
		return fmt.Errorf("%w: unknown scope %q", ErrWriteScope, scope)
	}
	return nil
}

// ValidateBodyScope is additional to, never a replacement for, edit validity.
// A metadata-only proposal must preserve the body or exactly retarget links.
func ValidateBodyScope(scope, path, original, proposed string, retarget func(string) (string, bool), exists func(string) bool) error {
	if err := ValidateAction(scope, "expand"); err != nil {
		return err
	}
	if scope != "metadata-links" {
		return nil
	}
	_, before, err := frontmatter.Parse(original)
	if err != nil {
		return err
	}
	_, after, err := frontmatter.Parse(proposed)
	if err != nil {
		return err
	}
	if after != before && after != links.Retarget(path, before, retarget, exists) {
		return fmt.Errorf("%w=metadata-links: body additions/rewrites require explicit write_scope=content or all", ErrWriteScope)
	}
	return nil
}

// ChangeTypes describes actual differences, not the producing model/stage.
// Categories are facets: graph-frontmatter changes have both metadata and links.
func ChangeTypes(path, action, original, proposed string, retarget func(string) (string, bool), exists func(string) bool) []string {
	if action != "expand" {
		return []string{"file"}
	}
	before, oldBody, oldErr := frontmatter.Parse(original)
	after, newBody, newErr := frontmatter.Parse(proposed)
	if oldErr != nil || newErr != nil {
		return []string{"unknown"}
	}
	delete(before, "kbm")
	delete(after, "kbm")
	out := []string{}
	if (len(before) != 0 || len(after) != 0) && !reflect.DeepEqual(before, after) {
		out = append(out, "frontmatter")
	}
	if !reflect.DeepEqual(before["document_id"], after["document_id"]) {
		out = append(out, "identity")
	}
	linkChange := !reflect.DeepEqual(before["related"], after["related"]) ||
		!reflect.DeepEqual(before["contradicted_by"], after["contradicted_by"]) ||
		!slices.Equal(links.InternalTargets(oldBody), links.InternalTargets(newBody))
	if oldBody != newBody {
		if newBody == links.Retarget(path, oldBody, retarget, exists) {
			linkChange = true
		} else {
			out = append(out, "body")
		}
	}
	if linkChange {
		out = append(out, "links")
	}
	return out
}
