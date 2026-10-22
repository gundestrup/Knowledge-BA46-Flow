package paths

// exclude.go holds the repository-relative exclusion patterns shared by
// config validation and the scanner walk. Patterns are anchored at the
// repository root and matched against forward-slash relative paths: '*',
// '?' and '[...]' work inside one path segment, and a whole '**' segment
// matches zero or more segments, so "drafts/**" covers the whole subtree.

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// ExcludePattern is one parsed exclude pattern as slash-separated segments.
type ExcludePattern []string

// ExcludePatterns is the parsed form of the exclude config list.
type ExcludePatterns []ExcludePattern

// ParseExcludePatterns parses and validates every pattern in raws.
func ParseExcludePatterns(raws []string) (ExcludePatterns, error) {
	out := make(ExcludePatterns, 0, len(raws))
	for _, raw := range raws {
		p, err := ParseExcludePattern(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// ParseExcludePattern validates one pattern and returns its segments. The
// pattern must stay inside the repository: absolute paths, empty segments and
// '.'/'..' segments are rejected, and '**' is only valid as a whole segment.
func ParseExcludePattern(raw string) (ExcludePattern, error) {
	p := strings.TrimRight(filepath.ToSlash(strings.TrimSpace(raw)), "/")
	if p == "" || filepath.IsAbs(raw) || filepath.IsAbs(p) {
		return nil, fmt.Errorf("invalid exclude pattern: %q", raw)
	}
	segs := strings.Split(p, "/")
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return nil, fmt.Errorf("invalid exclude pattern %q: empty, '.' and '..' segments are not allowed", raw)
		}
		if strings.Contains(s, "**") {
			if s != "**" {
				return nil, fmt.Errorf("invalid exclude pattern %q: '**' must be a whole path segment", raw)
			}
			continue
		}
		if _, err := path.Match(s, ""); err != nil {
			return nil, fmt.Errorf("invalid exclude pattern %q: %w", raw, err)
		}
	}
	return ExcludePattern(segs), nil
}

// Match reports whether rel — a forward-slash repository-relative path — is
// covered by any pattern. A matching directory prunes its whole subtree.
func (ps ExcludePatterns) Match(rel string) bool {
	if len(ps) == 0 || rel == "" {
		return false
	}
	name := strings.Split(filepath.ToSlash(rel), "/")
	for _, p := range ps {
		if matchSegments(p, name) {
			return true
		}
	}
	return false
}

// matchSegments matches pattern segments against path segments; '**'
// consumes zero or more path segments.
func matchSegments(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if matchSegments(pattern[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, err := path.Match(pattern[0], name[0])
		if err != nil || !ok {
			return false
		}
		pattern, name = pattern[1:], name[1:]
	}
	return len(name) == 0
}
