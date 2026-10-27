package frontmatter

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89aAbB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)

func DocumentID(fields map[string]any) (string, error) {
	value, present := fields["document_id"]
	if !present {
		return "", nil
	}
	id, ok := value.(string)
	if !ok || !uuidPattern.MatchString(id) {
		return "", fmt.Errorf("document_id must be a UUID string; identity is human/tool-owned")
	}
	return strings.ToLower(id), nil
}

func NewDocumentID() string {
	var raw [16]byte
	_, _ = rand.Read(raw[:]) // crypto/rand.Read never returns an error.
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[:4], raw[4:6], raw[6:8], raw[8:10], raw[10:])
}

// AddDocumentID is a byte-preserving insertion, not a YAML reserialization.
// It is used to prepare a reviewed proposal; scans never call it.
func AddDocumentID(content, id string) (string, error) {
	if !uuidPattern.MatchString(id) {
		return "", fmt.Errorf("invalid document UUID")
	}
	fields, _, err := Parse(content)
	if err != nil {
		return "", err
	}
	if _, present := fields["document_id"]; present {
		return "", fmt.Errorf("document_id already exists and must not be replaced")
	}
	trimmed := strings.TrimPrefix(content, "\uFEFF")
	bom := content[:len(content)-len(trimmed)]
	open := openFence.FindStringIndex(trimmed)
	var out string
	if open == nil {
		out = bom + "---\ndocument_id: \"" + strings.ToLower(id) + "\"\n---\n" + trimmed
	} else {
		eol := "\n"
		if strings.Contains(trimmed[:open[1]], "\r\n") {
			eol = "\r\n"
		}
		out = bom + trimmed[:open[1]] + "document_id: \"" + strings.ToLower(id) + "\"" + eol + trimmed[open[1]:]
	}
	if _, _, err := Parse(out); err != nil {
		return "", err
	}
	return out, nil
}

func ValidateIdentity(original, proposed string) error {
	fields, _, err := Parse(proposed)
	if err != nil {
		return err
	}
	id, err := DocumentID(fields)
	if err != nil || id == "" {
		return fmt.Errorf("identity initialization requires a valid document_id")
	}
	expected, err := AddDocumentID(original, id)
	if err != nil {
		return err
	}
	if Canonical(expected) != Canonical(proposed) {
		return fmt.Errorf("identity initialization may only insert document_id; no other metadata, links or body may change")
	}
	return nil
}

// PreserveIdentity carries the live document_id into proposed content the way
// PreserveManaged carries kbm: identity is tool/human-owned, so a proposal that
// omits the field keeps the current ID rather than deleting it. A conflicting
// or malformed proposed ID is an error; a proposed ID on an ID-less original
// is left for the origin validators (identity insertion vs. protected-add).
func PreserveIdentity(original, proposed string) (string, error) {
	before, _, err := Parse(original)
	if err != nil {
		return "", err
	}
	after, _, err := Parse(proposed)
	if err != nil {
		return "", err
	}
	oid, oerr := DocumentID(before)
	aid, aerr := DocumentID(after)
	if aerr != nil {
		return "", aerr
	}
	if oerr != nil || oid == aid {
		return proposed, nil // nothing to preserve, or already identical
	}
	if oid != "" && aid == "" {
		return AddDocumentID(proposed, oid)
	}
	if oid != "" && aid != "" {
		return "", fmt.Errorf("document_id must not change")
	}
	return proposed, nil
}
