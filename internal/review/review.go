// Package review handles the human decision step (list/interactive review/
// accept/reject) and applies accepted suggestions to the filesystem.
package review

import (
	"bufio"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"knowledge-ba46-flow/internal/config"
	"knowledge-ba46-flow/internal/database"
	"knowledge-ba46-flow/internal/frontmatter"
	"knowledge-ba46-flow/internal/mcppolicy"
	"knowledge-ba46-flow/internal/paths"
	"knowledge-ba46-flow/internal/proposal"
)

// List prints pending suggestions non-interactively (id, action, confidence,
// source file, target, reason).
func List(ctx context.Context, db *database.DB) error {
	sugs, err := db.PendingSuggestions(ctx)
	if err != nil {
		return err
	}
	if len(sugs) == 0 {
		fmt.Println("No pending suggestions.")
		return nil
	}
	for _, s := range sugs {
		fmt.Printf("\n#%d [%s %.0f%%]\n  file: %s\n  target: %s\n  reason: %s\n",
			s.ID, s.Action, s.Confidence*100, s.DocumentPath, cmp.Or(s.TargetPath, "-"), s.Reason)
	}
	return nil
}

// Interactive walks each pending suggestion and prompts per item:
// y/yes = accept, n/no = reject, q = quit, anything else = skip
// (leaves it pending for next time).
func Interactive(ctx context.Context, db *database.DB) error {
	sugs, err := db.PendingSuggestions(ctx)
	if err != nil {
		return err
	}
	in := bufio.NewScanner(os.Stdin)
	for _, s := range sugs {
		fmt.Printf("\n#%d [%s %.0f%%]\nfile: %s\ntarget: %s\nreason: %s\nAccept [y], reject [n], skip [s], quit [q]: ",
			s.ID, s.Action, s.Confidence*100, s.DocumentPath, cmp.Or(s.TargetPath, "-"), s.Reason)
		if !in.Scan() {
			break
		}
		switch strings.ToLower(strings.TrimSpace(in.Text())) {
		case "y", "yes":
			if err = db.Decide(ctx, s.ID, "accepted"); err != nil {
				return err
			}
		case "n", "no":
			if err = db.Decide(ctx, s.ID, "rejected"); err != nil {
				return err
			}
		case "q":
			return nil
		}
	}
	return in.Err()
}

// Decide is the non-interactive variant behind `KBM-flow accept ID` / `KBM-flow reject ID`.
func Decide(ctx context.Context, db *database.DB, idText, status string) error {
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid id: %w", err)
	}
	return db.Decide(ctx, id, status)
}

// ApplyResult reports the outcome of one applied suggestion: Applied means the
// filesystem operation succeeded; Error carries either that failure or a
// failure to record the outcome in SQLite.
type ApplyResult struct {
	ID      int64  `json:"id"`
	Action  string `json:"action"`
	Status  string `json:"status"` // applied | failed | superseded
	Applied bool   `json:"applied"`
	Error   string `json:"error,omitempty"`
}

// Apply executes every accepted suggestion whose source still matches the hash
// analyzed by the LLM, and returns a per-suggestion result. Stale suggestions
// become superseded; other failures are marked failed and do not stop the loop.
func Apply(ctx context.Context, c config.Config, db *database.DB) ([]ApplyResult, error) {
	sugs, err := db.Accepted(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ApplyResult, 0, len(sugs))
	applied := 0
	for _, s := range sugs {
		r := applySuggestion(ctx, c, db, s)
		if r.Applied {
			applied++
		}
		out = append(out, r)
	}
	if applied > 0 && c.ReindexTriggerPath != "" {
		out = append(out, touchReindexTrigger(c))
	}
	return out, nil
}

// touchReindexTrigger truncates the configured trigger file so its mtime
// signals "content changed" to the MCP server's reindex watcher. It stays
// inside the repository jail (the path was validated repo-relative at Load)
// and is reported as a result row so a failure surfaces in the apply output
// rather than silently leaving a stale index.
func touchReindexTrigger(c config.Config) ApplyResult {
	r := ApplyResult{Action: "reindex-trigger", Status: "failed"}
	rel, err := paths.CleanRelative(c.ReindexTriggerPath)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	root, err := os.OpenRoot(c.RepositoryPath)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	defer root.Close()
	f, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	if err := f.Close(); err != nil {
		r.Error = err.Error()
		return r
	}
	r.Status = "applied"
	r.Applied = true
	return r
}

// applySuggestion verifies and executes one accepted suggestion, recording the
// outcome in SQLite. Stale sources become superseded; failures are reported
// in the result and do not stop the apply loop.
func applySuggestion(ctx context.Context, c config.Config, db *database.DB, s database.Suggestion) ApplyResult {
	r := ApplyResult{ID: s.ID, Action: s.Action, Status: "failed"}
	sourceHash, err := hashSource(c.RepositoryPath, s.DocumentPath)
	if err != nil {
		r.Error = markApplyFailed(ctx, db, s.ID, err)
		return r
	}
	if sourceHash != s.SourceSHA256 {
		r.Status = "superseded"
		r.Error = "source document changed after analysis; suggestion marked superseded"
		if merr := db.MarkSuperseded(ctx, s.ID); merr != nil {
			r.Error += "; also failed to mark superseded: " + merr.Error()
		}
		return r
	}
	if err := applyOne(c, s); err != nil {
		r.Error = markApplyFailed(ctx, db, s.ID, err)
		return r
	}
	r.Status = "applied"
	r.Applied = true
	if err := db.MarkApplied(ctx, s.ID); err != nil {
		r.Error = "applied on disk but could not be marked in database: " + err.Error()
	}
	return r
}

// markApplyFailed records err for the suggestion and returns the message to
// surface, extended when the status update itself fails.
func markApplyFailed(ctx context.Context, db *database.DB, id int64, err error) string {
	msg := err.Error()
	if merr := db.MarkApplyFailed(ctx, id, msg); merr != nil {
		msg += "; also failed to mark in database: " + merr.Error()
	}
	return msg
}

// applyOne performs the filesystem operation for a single suggestion:
//
//	move   - rename source to target_path; refuses to overwrite existing files
//	delete - rename into <repo>/<trash_dir>/<timestamp>/ — never a hard delete
//	expand - atomically overwrite the document with proposed_content
//	merge  - atomically write proposed_content to the existing target, then trash the source
//
// All file operations are rooted with os.Root so both lexical traversal and
// symlinks pointing outside the repository are rejected by the OS boundary.
func applyOne(c config.Config, s database.Suggestion) error {
	src, err := paths.CleanRelative(s.DocumentPath)
	if err != nil {
		return err
	}
	trashDir, err := cleanTrashDir(c.TrashDirectory)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(c.RepositoryPath)
	if err != nil {
		return err
	}
	defer root.Close()

	switch s.Action {
	case "move":
		if s.ProposedContent != "" {
			return fmt.Errorf("move must not contain proposed_content")
		}
		return applyMove(root, src, s.TargetPath)
	case "delete":
		if s.TargetPath != "" || s.ProposedContent != "" {
			return fmt.Errorf("delete must not contain a target or proposed_content")
		}
		return applyDelete(root, trashDir, src)
	case "expand":
		if s.TargetPath != "" {
			return fmt.Errorf("expand must have an empty target_path")
		}
		return applyExpand(root, src, s.ProposedContent, c)
	case "merge":
		return applyMerge(root, trashDir, src, s.TargetPath, s.ProposedContent, c)
	default:
		return fmt.Errorf("action cannot be applied: %s", s.Action)
	}
}

// cleanTrashDir validates the configured trash directory, defaulting to
// .kbm-trash when unset.
func cleanTrashDir(dir string) (string, error) {
	if dir == "" {
		dir = ".kbm-trash"
	}
	cleaned, err := paths.CleanRelative(dir)
	if err != nil {
		return "", fmt.Errorf("invalid trash_directory: %w", err)
	}
	return cleaned, nil
}

// applyMove renames src to the suggestion's target, refusing to overwrite.
func applyMove(root *os.Root, src, target string) error {
	if err := proposal.ValidateMove(src, target); err != nil {
		return err
	}
	dst, err := paths.CleanRelative(target)
	if err != nil {
		return err
	}
	if err := mcppolicy.AssertMutationAllowed(root, src, dst); err != nil {
		return err
	}
	if err := requireAbsent(root, dst, fmt.Sprintf("target already exists: %s", target)); err != nil {
		return err
	}
	if err = makeParent(root, dst); err != nil {
		return err
	}
	return root.Rename(src, dst)
}

// applyDelete renames src into the timestamped trash directory — never a
// hard delete.
func applyDelete(root *os.Root, trashDir, src string) error {
	if err := mcppolicy.AssertMutationAllowed(root, src); err != nil {
		return err
	}
	dst := trashPath(trashDir, src)
	if err := makeParent(root, dst); err != nil {
		return err
	}
	return root.Rename(src, dst)
}

// applyExpand atomically overwrites the source with the proposed content.
func applyExpand(root *os.Root, src, proposed string, c config.Config) error {
	if err := mcppolicy.AssertMutationAllowed(root, src); err != nil {
		return err
	}
	if strings.TrimSpace(proposed) == "" {
		return fmt.Errorf("missing proposed_content")
	}
	if err := checkProposedContent(root, src, proposed, c); err != nil {
		return err
	}
	return paths.AtomicWrite(root, src, []byte(proposed))
}

// applyMerge atomically writes the proposed content to the existing target,
// then moves the source into the trash.
func applyMerge(root *os.Root, trashDir, src, target, proposed string, c config.Config) error {
	if strings.TrimSpace(target) == "" || strings.TrimSpace(proposed) == "" {
		return fmt.Errorf("merge requires target_path and proposed_content")
	}
	dst, err := paths.CleanRelative(target)
	if err != nil {
		return err
	}
	if src == dst {
		return fmt.Errorf("source and merge target paths are the same")
	}
	if err := mcppolicy.AssertMutationAllowed(root, src, dst); err != nil {
		return err
	}
	if _, err = root.Lstat(dst); err != nil {
		return fmt.Errorf("merge target does not exist: %w", err)
	}
	for _, original := range []string{src, dst} {
		if err := checkProposedContent(root, original, proposed, c); err != nil {
			return err
		}
	}
	trash := trashPath(trashDir, src)
	if err = makeParent(root, trash); err != nil {
		return err
	}
	if err := requireAbsent(root, trash, fmt.Sprintf("trash target already exists: %s", trash)); err != nil {
		return err
	}
	if err = paths.AtomicWrite(root, dst, []byte(proposed)); err != nil {
		return err
	}
	return root.Rename(src, trash)
}

// requireAbsent fails with existsError when path is already taken inside root;
// missing paths are fine and other stat errors propagate.
func requireAbsent(root *os.Root, path, existsError string) error {
	if _, err := root.Lstat(path); err == nil {
		return errors.New(existsError)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// trashPath returns the repository-relative destination for a delete or merge
// source. trashDir has already passed cleanRelative.
func trashPath(trashDir, source string) string {
	return filepath.Join(trashDir, time.Now().UTC().Format("20060102T150405.000000000Z"), source)
}

// hashSource verifies that an accepted suggestion still describes the current
// source file. The read is rooted to the repository so this check cannot follow
// an external symlink.
func hashSource(repositoryPath, rel string) (string, error) {
	local, err := paths.CleanRelative(rel)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(repositoryPath)
	if err != nil {
		return "", err
	}
	defer root.Close()
	file, err := root.Open(local)
	if err != nil {
		return "", err
	}
	defer file.Close()
	raw, err := io.ReadAll(file)
	if err != nil {
		return "", err
	}
	// Canonical content: the tool-owned kbm block does not invalidate a
	// suggestion the way real content edits do.
	hash := sha256.New()
	hash.Write([]byte(frontmatter.Canonical(string(raw))))
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// safePath is retained for path validation callers/tests. Filesystem mutations
// must use os.Root via applyOne, not this joined path alone.
func safePath(root, rel string) (string, error) {
	local, err := paths.CleanRelative(rel)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, local), nil
}

func makeParent(root *os.Root, path string) error {
	parent := filepath.Dir(path)
	if parent == "." {
		return nil
	}
	return root.MkdirAll(parent, 0755)
}

// PurgeTrash permanently deletes trash entries — the timestamped directories
// left by delete/merge applies — whose modification time predates `cutoff`.
// Listing and deletion stay confined to the repository via os.Root, so a
// symlinked trash entry cannot escape. A missing trash directory is fine.
func PurgeTrash(c config.Config, cutoff time.Time) (int, error) {
	trashDir := c.TrashDirectory
	if trashDir == "" {
		trashDir = ".kbm-trash"
	}
	trashDir, err := paths.CleanRelative(trashDir)
	if err != nil {
		return 0, fmt.Errorf("invalid trash_directory: %w", err)
	}
	root, err := os.OpenRoot(c.RepositoryPath)
	if err != nil {
		return 0, err
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), filepath.ToSlash(trashDir))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			return removed, err
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if err := root.RemoveAll(filepath.Join(trashDir, e.Name())); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
