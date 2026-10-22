// Package paths centralizes repository-relative path validation shared by the
// scanner, review and mcppolicy packages. Keeping one implementation matters:
// callers rely on this check before opening paths under os.Root, so divergent
// copies would quietly weaken traversal protection.
package paths

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// CleanRelative rejects empty, absolute, root and '..' paths and returns the
// cleaned repository-relative form. os.Root performs the additional
// symlink-aware containment check when the path is opened.
func CleanRelative(raw string) (string, error) {
	if raw == "" || filepath.IsAbs(raw) {
		return "", fmt.Errorf("invalid relative path: %q", raw)
	}
	for _, part := range strings.Split(filepath.ToSlash(raw), "/") {
		if part == ".." {
			return "", fmt.Errorf("path escapes repository: %q", raw)
		}
	}
	local := filepath.Clean(filepath.FromSlash(raw))
	if local == "." || !filepath.IsLocal(local) {
		return "", fmt.Errorf("path escapes repository: %q", raw)
	}
	return local, nil
}

// AtomicWrite creates a unique temporary sibling, writes it fully, then
// renames it over the target. The os.Root confines both the temporary file
// and the rename to the repository, including when a parent path contains a
// symlink. The target must already exist as a regular file.
func AtomicWrite(root *os.Root, path string, data []byte) error {
	info, err := root.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("target is not a regular file: %s", path)
	}
	mode := info.Mode().Perm()
	parent := filepath.Dir(path)

	for attempt := 0; attempt < 5; attempt++ {
		random := make([]byte, 12)
		if _, err := rand.Read(random); err != nil {
			return err
		}
		temp := filepath.Join(parent, ".kbm-"+hex.EncodeToString(random)+".tmp")
		f, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if _, err = io.Copy(f, bytes.NewReader(data)); err != nil {
			_ = f.Close()
			_ = root.Remove(temp)
			return err
		}
		if err = f.Close(); err != nil {
			_ = root.Remove(temp)
			return err
		}
		if err = root.Rename(temp, path); err != nil {
			_ = root.Remove(temp)
			return err
		}
		return nil
	}
	return fmt.Errorf("could not allocate a temporary file for %s", path)
}
