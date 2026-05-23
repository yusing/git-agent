package trace

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Recorder struct {
	dir string
	mu  sync.Mutex
	seq int
}

func New(root, command string) (*Recorder, error) {
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	dir := filepath.Join(root, ".git-agent", "sessions", stamp+"-"+command)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Recorder{dir: dir}, nil
}

func (r *Recorder) Dir() string {
	if r == nil {
		return ""
	}
	return r.dir
}

func (r *Recorder) Write(kind string, value any) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	path := filepath.Join(r.dir, fmt.Sprintf("%03d-%s.json", r.seq, kind))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(Normalize(value))
}

func Normalize(value any) any {
	data, err := json.Marshal(value)
	if err != nil {
		return value
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var normalized any
	if err := decoder.Decode(&normalized); err != nil {
		return value
	}
	return expandJSONStrings(normalized)
}

func expandJSONStrings(value any) any {
	switch value := value.(type) {
	case map[string]any:
		for key, item := range value {
			value[key] = expandJSONStrings(item)
		}
		return value
	case []any:
		for idx, item := range value {
			value[idx] = expandJSONStrings(item)
		}
		return value
	case string:
		return expandJSONString(value)
	default:
		return value
	}
}

func expandJSONString(value string) any {
	trimmed := bytes.TrimSpace([]byte(value))
	if len(trimmed) == 0 {
		return value
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return value
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	var expanded any
	if err := decoder.Decode(&expanded); err != nil {
		return value
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return value
	}
	return expandJSONStrings(expanded)
}
