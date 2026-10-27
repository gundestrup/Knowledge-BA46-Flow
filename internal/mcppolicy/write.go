// Package mcppolicy implements the Knowledge Base MCP Server's local write
// policy for filesystem mutations performed by KBM-flow.
package mcppolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"knowledge-ba46-flow/internal/paths"
)

const (
	writePolicyFilename = ".kb-policy.json"
	maxWritePolicyBytes = 16 * 1024
)

var ErrDenied = errors.New("repository mutation policy blocks this write")

// AssertMutationAllowed checks the write policy for the repository root and
// each top-level shelf touched by the operation. This supports configuring
// repository_path as either one shelf or the MCP server's shelves directory.
// Missing policy files allow writes; malformed policies fail closed.
func AssertMutationAllowed(root *os.Root, targets ...string) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: %w", ErrDenied, err)
		}
	}()
	scopes := map[string]bool{".": true}
	normalized := make([]string, 0, len(targets))
	for _, raw := range targets {
		local, err := paths.CleanRelative(raw)
		if err != nil {
			return err
		}
		normalized = append(normalized, filepath.ToSlash(local))
		parts := strings.Split(filepath.ToSlash(local), "/")
		if len(parts) > 1 {
			scopes[parts[0]] = true
		}
	}

	scopeNames := make([]string, 0, len(scopes))
	for scope := range scopes {
		scopeNames = append(scopeNames, scope)
	}
	slices.Sort(scopeNames)
	for _, scope := range scopeNames {
		policyPath := filepath.ToSlash(filepath.Join(scope, writePolicyFilename))
		for _, operationPath := range normalized {
			if operationPath == policyPath {
				return fmt.Errorf("%s is managed policy and cannot be changed by KBM-flow", policyPath)
			}
		}
		denied, err := policyDeniesMutations(root, filepath.FromSlash(policyPath))
		if err != nil {
			return err
		}
		if denied {
			return fmt.Errorf("MCP write policy at %s denies mutations", policyPath)
		}
	}
	return nil
}

func policyDeniesMutations(root *os.Root, policyPath string) (bool, error) {
	info, err := root.Lstat(policyPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read MCP write policy %s: %w", policyPath, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("invalid MCP write policy at %s: expected a regular file; writes are denied", policyPath)
	}

	file, err := root.Open(policyPath)
	if err != nil {
		return false, fmt.Errorf("read MCP write policy %s: %w", policyPath, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxWritePolicyBytes+1))
	if err != nil {
		return false, fmt.Errorf("read MCP write policy %s: %w", policyPath, err)
	}
	if len(data) > maxWritePolicyBytes {
		return false, fmt.Errorf("invalid MCP write policy at %s: file exceeds %d bytes; writes are denied", policyPath, maxWritePolicyBytes)
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return false, invalidPolicy(policyPath, err)
	}
	for key := range object {
		if key != "mutations" {
			return false, invalidPolicy(policyPath, fmt.Errorf("unknown key %q", key))
		}
	}

	raw, exists := object["mutations"]
	if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false, nil
	}
	var mutations string
	if err := json.Unmarshal(raw, &mutations); err != nil {
		return false, invalidPolicy(policyPath, err)
	}
	switch mutations {
	case "allow":
		return false, nil
	case "deny":
		return true, nil
	default:
		return false, invalidPolicy(policyPath, fmt.Errorf(`mutations must be "allow" or "deny"`))
	}
}

func invalidPolicy(path string, cause error) error {
	if cause == nil {
		cause = fmt.Errorf("expected a JSON object")
	}
	return fmt.Errorf("invalid MCP write policy at %s: %v; writes are denied until the policy is fixed or removed", path, cause)
}
