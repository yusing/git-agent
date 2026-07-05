// Package metadata resolves and migrates git-agent metadata directories.
package metadata

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const dirName = ".git-agent"

// Dir returns the per-project metadata directory, migrating the legacy
// repository-local directory when it exists.
func Dir(projectRoot string) (string, error) {
	root, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, dirName, PathSHA(root))
	if err := migrate(filepath.Join(root, dirName), dir); err != nil {
		return "", err
	}
	return dir, nil
}

// PathSHA returns the SHA-256 hex digest for a cleaned project path.
func PathSHA(path string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	return hex.EncodeToString(sum[:])
}

func migrate(legacyDir, dir string) error {
	info, err := os.Stat(legacyDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("legacy metadata path %s is not a directory", legacyDir)
	}
	if sameOrContainsPath(legacyDir, dir) || sameExistingPath(legacyDir, filepath.Dir(dir)) {
		return nil
	}
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return err
		}
		if err := os.Rename(legacyDir, dir); err == nil {
			return nil
		}
	} else if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := copyDir(legacyDir, dir); err != nil {
		return err
	}
	return os.RemoveAll(legacyDir)
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == src {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode().IsRegular():
			return copyFile(path, target, info.Mode().Perm())
		default:
			return fmt.Errorf("legacy metadata path %s has unsupported file mode %s", path, info.Mode())
		}
	})
}

func copyFile(src, dst string, mode fs.FileMode) error {
	if _, err := os.Stat(dst); err == nil {
		next, err := conflictPath(dst)
		if err != nil {
			return err
		}
		dst = next
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	return errors.Join(copyErr, closeErr)
}

func conflictPath(path string) (string, error) {
	dir, base := filepath.Split(path)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	suffix := ".legacy-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	for i := range 100 {
		candidate := filepath.Join(dir, stem+suffix+conflictSuffix(i)+ext)
		if _, err := os.Stat(candidate); errors.Is(err, fs.ErrNotExist) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("could not find conflict path for %s", path)
}

func conflictSuffix(i int) string {
	if i == 0 {
		return ""
	}
	return fmt.Sprintf("-%d", i)
}

func sameOrContainsPath(parent, child string) bool {
	parentAbs, parentErr := filepath.Abs(parent)
	childAbs, childErr := filepath.Abs(child)
	if parentErr != nil || childErr != nil {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(parentAbs), filepath.Clean(childAbs))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func sameExistingPath(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}
