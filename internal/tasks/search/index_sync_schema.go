package search

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	json "encoding/json/v2"

	"github.com/yusing/git-agent/internal/jsonx"
)

const (
	indexSyncSchemaV1 = 1
	indexSyncSchemaV2 = 2
)

type indexSyncSchema struct {
	Version int `json:"version"`
}

func decodeStrictJSON(data []byte, value any) error {
	if err := json.Unmarshal(data, value, json.RejectUnknownMembers(true)); err != nil {
		if jsonx.ExtraJSON(err) {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func readIndexSyncSchema(root string) (indexSyncSchema, error) {
	data, err := os.ReadFile(filepath.Join(root, "schema.json"))
	if err != nil {
		return indexSyncSchema{}, err
	}
	var schema indexSyncSchema
	if err := decodeStrictJSON(data, &schema); err != nil {
		return indexSyncSchema{}, fmt.Errorf("parse index sync schema: %w", err)
	}
	if schema.Version != indexSyncSchemaV1 && schema.Version != indexSyncSchemaV2 {
		return indexSyncSchema{}, fmt.Errorf("unsupported index sync schema version %d", schema.Version)
	}
	return schema, nil
}

func writeIndexSyncSchema(root string, version int) error {
	if version != indexSyncSchemaV1 && version != indexSyncSchemaV2 {
		return fmt.Errorf("unsupported index sync schema version %d", version)
	}
	temporary, err := os.CreateTemp(root, ".schema-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := fmt.Fprintf(temporary, "{\"version\":%d}\n", version); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, filepath.Join(root, "schema.json")); err != nil {
		return err
	}
	return syncDirectory(root)
}

func syncTreeHasData(root string) (bool, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.Name() != ".git" {
			return true, nil
		}
	}
	return false, nil
}

func validateSyncTreeForSchema(root string, version int) error {
	return walkSafeSyncTree(root, func(path, rel string, directory bool) error {
		if validSyncTreeEntryForSchema(rel, directory, version) {
			return nil
		}
		return fmt.Errorf("index sync repository contains unsafe path %s", path)
	})
}

func walkSafeSyncTree(root string, visit func(path, rel string, directory bool) error) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("index sync repository contains symlink %s", path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == ".git" && entry.IsDir() {
			return filepath.SkipDir
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("index sync repository contains non-regular file %s", path)
		}
		return visit(path, rel, entry.IsDir())
	})
}

func validSyncTreeEntryForSchema(rel string, directory bool, version int) bool {
	if rel == "." {
		return directory
	}
	if rel == "schema.json" {
		return !directory
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 0 {
		return false
	}
	if parts[0] == "indexes" {
		if len(parts) == 1 {
			return directory
		}
		if !canonicalLowerHex(parts[1], 64) {
			return false
		}
		if len(parts) == 2 {
			return directory
		}
		if !canonicalObjectID(parts[2]) {
			return false
		}
		if len(parts) == 3 {
			return directory
		}
		if len(parts) != 4 || directory || !strings.HasSuffix(parts[3], ".json") {
			return false
		}
		modelKey := strings.TrimSuffix(parts[3], ".json")
		switch version {
		case indexSyncSchemaV1:
			return canonicalLowerHex(modelKey, 16)
		case indexSyncSchemaV2:
			return canonicalLowerHex(modelKey, 64)
		default:
			return false
		}
	}
	if parts[0] != "packs" || version != indexSyncSchemaV2 {
		return false
	}
	if len(parts) == 1 {
		return directory
	}
	if !canonicalLowerHex(parts[1], 64) {
		return false
	}
	if len(parts) == 2 {
		return directory
	}
	return len(parts) == 3 && !directory && strings.HasSuffix(parts[2], ".pack") &&
		canonicalLowerHex(strings.TrimSuffix(parts[2], ".pack"), 64)
}
