package search

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	indexSyncSchemaV1 = 1
	indexSyncSchemaV2 = 2
)

type indexSyncSchema struct {
	Version int `json:"version"`
}

func decodeStrictJSON(data []byte, value any) error {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := checkJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func checkJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		keys := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			if keys[key] {
				return fmt.Errorf("duplicate JSON object key %q", key)
			}
			keys[key] = true
			if err := checkJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("JSON object is not closed")
		}
	case '[':
		for decoder.More() {
			if err := checkJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("JSON array is not closed")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
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
	return os.WriteFile(filepath.Join(root, "schema.json"), []byte(fmt.Sprintf("{\"version\":%d}\n", version)), 0o600)
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
		if validSyncTreeEntryForSchema(rel, entry.IsDir(), version) {
			return nil
		}
		return fmt.Errorf("index sync repository contains unsafe path %s", path)
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
