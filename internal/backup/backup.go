// Package backup provides offline, checksummed backups. Holding the central
// bbolt read lock excludes a running server for the entire copy.
package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

const manifestName = "backup-manifest.json"

type manifest struct {
	Version int               `json:"version"`
	Created time.Time         `json:"created"`
	Files   map[string]string `json:"files"`
}

// Create requires a stopped server and a new destination outside the data tree.
func Create(dataDir, destination string) error {
	root, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		return err
	}
	dest, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(dest))
	if err != nil {
		return err
	}
	dest = filepath.Join(resolvedParent, filepath.Base(dest))
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, dest)
	if err != nil || rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return errors.New("backup destination must be outside data directory")
	}
	if err := rejectLinks(root); err != nil {
		return err
	}
	db, err := bolt.Open(filepath.Join(root, "stiva.db"), 0600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		return fmt.Errorf("stop Stiva before backup: %w", err)
	}
	defer db.Close()
	if err := checkDB(db); err != nil {
		return err
	}
	return publish(root, dest, true)
}

// Verify checks every file, rejects extra files and symlinks, and checks bbolt
// page integrity. Checksums detect corruption; they do not authenticate backups.
func Verify(source string) error {
	if err := rejectLinks(source); err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(source, manifestName))
	if err != nil {
		return err
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	if m.Version != 1 || len(m.Files) == 0 {
		return errors.New("unsupported or empty backup manifest")
	}
	actual, err := hashes(source)
	if err != nil {
		return err
	}
	if len(actual) != len(m.Files) {
		return errors.New("backup file count mismatch")
	}
	for path, sum := range actual {
		if m.Files[path] != sum {
			return fmt.Errorf("backup checksum mismatch: %s", path)
		}
	}
	if _, ok := actual["stiva.db"]; !ok {
		return errors.New("missing central metadata database")
	}
	return filepath.WalkDir(source, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if d.IsDir() || (rel != "stiva.db" && (filepath.Dir(rel) != "metadata" || !strings.HasSuffix(rel, ".db"))) {
			return nil
		}
		db, err := bolt.Open(path, 0600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
		if err != nil {
			return err
		}
		defer db.Close()
		return checkDB(db)
	})
}

// Restore never overwrites an existing destination. Verify before copying and
// again on the staged copy to detect source changes during restoration.
func Restore(source, destination string) error {
	if err := Verify(source); err != nil {
		return err
	}
	return publish(source, destination, false)
}

func checkDB(db *bolt.DB) error {
	return db.View(func(tx *bolt.Tx) error {
		var errs []error
		for err := range tx.Check() {
			errs = append(errs, err)
		}
		return errors.Join(errs...)
	})
}

func rejectLinks(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 || (!d.IsDir() && !d.Type().IsRegular()) {
			return fmt.Errorf("unsupported file: %s", path)
		}
		return nil
	})
}

func hashes(root string) (map[string]string, error) {
	scoped, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	defer scoped.Close()
	result := make(map[string]string)
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == manifestName {
			return nil
		}
		f, err := scoped.Open(rel)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return err
		}
		result[filepath.ToSlash(rel)] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	return result, err
}

func publish(source, destination string, create bool) error {
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("destination must not exist: %s", destination)
	}
	parent := filepath.Dir(destination)
	staging, err := os.MkdirTemp(parent, ".stiva-backup-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	inputRoot, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer inputRoot.Close()
	outputRoot, err := os.OpenRoot(staging)
	if err != nil {
		return err
	}
	defer outputRoot.Close()
	err = filepath.WalkDir(source, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			return outputRoot.MkdirAll(rel, 0700)
		}
		if rel == manifestName && create {
			return errors.New("data directory contains a backup manifest")
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("unsupported file: %s", path)
		}
		in, err := inputRoot.Open(rel)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := outputRoot.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		syncErr := out.Sync()
		closeErr := out.Close()
		return errors.Join(copyErr, syncErr, closeErr)
	})
	if err != nil {
		return err
	}
	if create {
		files, err := hashes(staging)
		if err != nil {
			return err
		}
		raw, err := json.MarshalIndent(manifest{Version: 1, Created: time.Now().UTC(), Files: files}, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(staging, manifestName), raw, 0600); err != nil {
			return err
		}
	}
	if err := Verify(staging); err != nil {
		return err
	}
	if !create {
		if err := os.Remove(filepath.Join(staging, manifestName)); err != nil {
			return err
		}
	}
	if err := syncTree(staging); err != nil {
		return err
	}
	if err := outputRoot.Close(); err != nil {
		return err
	}
	if err := os.Rename(staging, destination); err != nil {
		return err
	}
	return syncDir(parent)
}

func syncTree(root string) error {
	scoped, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer scoped.Close()
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return syncDir(path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		f, err := scoped.OpenFile(rel, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		defer f.Close()
		return f.Sync()
	})
}
