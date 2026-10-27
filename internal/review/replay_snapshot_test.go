package review

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knowledge-ba46-flow/internal/config"

	"modernc.org/sqlite"
)

// Do not use database.Open on the source: it changes journal mode and migrates.
// SQLite's online backup reads a consistent snapshot, including committed WAL.
func snapshotReplayDB(ctx context.Context, source, destination string) error {
	f, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	uri := &url.URL{Scheme: "file", Path: source, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(raw any) (err error) {
		backuper, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return fmt.Errorf("SQLite driver has no online backup API")
		}
		target := &url.URL{Scheme: "file", Path: destination, RawQuery: "mode=rw"}
		backup, err := backuper.NewBackup(target.String())
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, backup.Finish()) }()
		if err := ctx.Err(); err != nil {
			return err
		}
		more, err := backup.Step(-1)
		if err == nil && more {
			return fmt.Errorf("SQLite backup did not finish")
		}
		return err
	})
}

type replayFiles struct {
	Hashes          map[string][sha256.Size]byte
	SkippedSymlinks int
}

// Only document/validation files are needed. Do not copy Git, credentials,
// indexes, runtime state or symlinks into a private production-data fixture.
func snapshotReplayFiles(c config.Config, destination string) (replayFiles, error) {
	out := replayFiles{Hashes: map[string][sha256.Size]byte{}}
	source, err := filepath.EvalSymlinks(c.RepositoryPath)
	if err != nil {
		return out, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(destination))
	if err != nil {
		return out, err
	}
	rel, err := filepath.Rel(source, filepath.Join(parent, filepath.Base(destination)))
	if err != nil || rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return out, fmt.Errorf("replay destination must be outside the source repository")
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return out, err
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return out, err
	}
	defer root.Close()
	extensions := map[string]bool{}
	for _, ext := range c.Extensions {
		extensions[strings.ToLower(ext)] = true
	}
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			out.SkippedSymlinks++
			return nil
		}
		if entry.IsDir() {
			if path != "." && (strings.HasPrefix(entry.Name(), ".") || path == c.TrashDirectory) {
				return fs.SkipDir
			}
			return nil
		}
		support := entry.Name() == ".kb-policy.json" || path == "taxonomy.yaml"
		if !support && (strings.HasPrefix(entry.Name(), ".") || !extensions[strings.ToLower(filepath.Ext(path))]) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		content, err := readReplayFile(root, path, max(c.MaxDocumentBytes, 1<<20))
		if err != nil {
			return err
		}
		target := filepath.Join(destination, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(target, content, 0600); err != nil {
			return err
		}
		out.Hashes[path] = sha256.Sum256(content)
		return nil
	})
	return out, err
}

func readReplayFile(root *os.Root, path string, limit int64) ([]byte, error) {
	file, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("replay source is not a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err == nil && int64(len(raw)) > limit {
		err = fmt.Errorf("replay source exceeds the bounded snapshot limit")
	}
	return raw, err
}

func TestReplayBackupIncludesWALWithoutChangingSource(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source.db")
	writer, err := sql.Open("sqlite", source)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec(`PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0;
CREATE TABLE replay(value TEXT); INSERT INTO replay VALUES('committed in WAL'); PRAGMA user_version=11;`); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	wal, err := os.Stat(source + "-wal")
	if err != nil || wal.Size() == 0 {
		t.Fatalf("fixture must have committed WAL data: %v", err)
	}
	destination := filepath.Join(t.TempDir(), "copy.db")
	if err := snapshotReplayDB(t.Context(), source, destination); err != nil {
		t.Fatal(err)
	}
	copy, err := sql.Open("sqlite", destination)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	var got string
	if err := copy.QueryRow(`SELECT value FROM replay`).Scan(&got); err != nil || got != "committed in WAL" {
		t.Fatalf("backup missed WAL data: value=%q err=%v", got, err)
	}
	after, err := os.ReadFile(source)
	if err != nil || sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatalf("source database changed: %v", err)
	}
	if err := snapshotReplayDB(t.Context(), source, source); err == nil {
		t.Fatal("backup must not overwrite an existing file or the source")
	}
}

func TestReplayCopyPreservesPolicyAndSkipsSensitiveState(t *testing.T) {
	source := t.TempDir()
	for name, content := range map[string]string{
		"note.md": "# Note\n", "taxonomy.yaml": "kind: [repo]\n",
		".kb-policy.json": `{"mutations":"deny"}`,
		".env":            "DO_NOT_COPY=secret", ".kbm/state.db": "runtime state",
	} {
		target := filepath.Join(source, name)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(source, ".env"), filepath.Join(source, "alias.md")); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "repository")
	files, err := snapshotReplayFiles(config.Config{RepositoryPath: source, Extensions: []string{".md"}}, destination)
	if err != nil || len(files.Hashes) != 3 || files.SkippedSymlinks != 1 {
		t.Fatalf("files=%+v err=%v", files, err)
	}
	policy, _ := os.ReadFile(filepath.Join(destination, ".kb-policy.json"))
	if string(policy) != `{"mutations":"deny"}` {
		t.Fatalf("mutation policy changed: %q", policy)
	}
	for _, path := range []string{".env", ".kbm", "alias.md"} {
		if _, err := os.Lstat(filepath.Join(destination, path)); !os.IsNotExist(err) {
			t.Fatalf("unexpected copied path %s: %v", path, err)
		}
	}
	if _, err := snapshotReplayFiles(config.Config{RepositoryPath: source}, filepath.Join(source, "nested")); err == nil {
		t.Fatal("snapshot must not be created inside the live source")
	}
}
