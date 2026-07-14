// Package background persists durable review and simplification task results.
package background

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yusing/git-agent/internal/trace"
)

const (
	recordVersion       = 1
	defaultPollInterval = 100 * time.Millisecond
	heartbeatInterval   = 5 * time.Second
	heartbeatStaleAfter = 30 * time.Second
	maxRecordBytes      = 4 << 20
)

// Record is the versioned durable state of one background task.
type Record struct {
	Version   int          `json:"version"`
	ID        string       `json:"id"`
	Command   string       `json:"command"`
	PID       int          `json:"pid"`
	StartedAt time.Time    `json:"started_at"`
	UpdatedAt time.Time    `json:"updated_at"`
	Terminal  *trace.Event `json:"terminal,omitempty"`
}

// Store owns background records beneath one project metadata directory.
type Store struct {
	dir          string
	pollInterval time.Duration
	processAlive func(int) bool
	now          func() time.Time
}

// NewStore opens the owner-only background record directory.
func NewStore(metadataDir string) (*Store, error) {
	dir := filepath.Join(metadataDir, "background")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create background record directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure background record directory: %w", err)
	}
	return &Store{
		dir:          dir,
		pollInterval: defaultPollInterval,
		processAlive: processIsAlive,
		now:          time.Now,
	}, nil
}

// Heartbeat refreshes a running record until ctx is canceled.
func (s *Store) Heartbeat(ctx context.Context, id string) error {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			if err := s.touch(id, now); err != nil {
				return err
			}
		}
	}
}

func (s *Store) touch(id string, now time.Time) error {
	return s.withRecordLock(id, func(path string) error {
		record, err := s.readPath(path, id)
		if err != nil {
			return err
		}
		if record.Terminal != nil {
			return nil
		}
		record.UpdatedAt = now.UTC()
		return s.writeRecord(path, record)
	})
}

// NewID returns an opaque task identifier with at least 128 bits of entropy.
func NewID() string {
	return rand.Text()
}

// Create atomically persists a running record without replacing an existing ID.
func (s *Store) Create(id, command string, pid int, now time.Time) error {
	if err := validateID(id); err != nil {
		return err
	}
	if err := validateCommand(command); err != nil {
		return err
	}
	if pid <= 0 {
		return errors.New("background task PID must be positive")
	}
	if now.IsZero() {
		return errors.New("background task start time is required")
	}
	record := Record{
		Version:   recordVersion,
		ID:        id,
		Command:   command,
		PID:       pid,
		StartedAt: now.UTC(),
		UpdatedAt: now.UTC(),
	}
	return s.withRecordLock(id, func(path string) error {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("background task %s already exists", id)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("inspect background task %s: %w", id, err)
		}
		return s.writeRecord(path, record)
	})
}

// Complete atomically adds the exact terminal final or error event.
func (s *Store) Complete(id string, terminal trace.Event, now time.Time) error {
	if terminal.Kind != "final" && terminal.Kind != "error" {
		return fmt.Errorf("background terminal event kind %q is invalid", terminal.Kind)
	}
	if now.IsZero() {
		return errors.New("background task update time is required")
	}
	return s.withRecordLock(id, func(path string) error {
		record, err := s.readPath(path, id)
		if err != nil {
			return err
		}
		if record.Terminal != nil {
			return fmt.Errorf("background task %s is already complete", id)
		}
		record.UpdatedAt = now.UTC()
		record.Terminal = &terminal
		return s.writeRecord(path, record)
	})
}

// Read returns one complete, validated record snapshot.
func (s *Store) Read(id string) (Record, error) {
	if err := validateID(id); err != nil {
		return Record{}, err
	}
	return s.readPath(filepath.Join(s.dir, id+".json"), id)
}

// Wait blocks until a matching task reaches a terminal event. It returns only
// the final event's text value; terminal errors remain errors.
func (s *Store) Wait(ctx context.Context, id, command string) (any, error) {
	if err := validateCommand(command); err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		record, err := s.Read(id)
		if err != nil {
			return nil, err
		}
		if record.Command != command {
			return nil, fmt.Errorf("background task %s belongs to %s, not %s", id, record.Command, command)
		}
		if record.Terminal != nil {
			switch record.Terminal.Kind {
			case "final":
				text, ok := record.Terminal.Value["text"]
				if !ok || text == nil {
					return nil, fmt.Errorf("background task %s final event has no text", id)
				}
				return text, nil
			case "error":
				message, _ := record.Terminal.Value["message"].(string)
				if strings.TrimSpace(message) == "" {
					message = "producer reported an error"
				}
				return nil, fmt.Errorf("background %s task %s failed: %s", command, id, message)
			}
		}
		if !s.processAlive(record.PID) {
			return nil, fmt.Errorf("background %s task %s producer PID %d is not running", command, id, record.PID)
		}
		if s.now().Sub(record.UpdatedAt) > heartbeatStaleAfter {
			return nil, fmt.Errorf("background %s task %s producer heartbeat is stale", command, id)
		}
		timer := time.NewTimer(s.pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *Store) readPath(path, id string) (Record, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, fmt.Errorf("unknown background task %s", id)
	}
	if err != nil {
		return Record{}, fmt.Errorf("inspect background task %s: %w", id, err)
	}
	if !info.Mode().IsRegular() {
		return Record{}, fmt.Errorf("background task %s record is not a regular file", id)
	}
	if info.Size() > maxRecordBytes {
		return Record{}, fmt.Errorf("background task %s record exceeds %d bytes", id, maxRecordBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return Record{}, fmt.Errorf("open background task %s: %w", id, err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxRecordBytes))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	var record Record
	if err := decoder.Decode(&record); err != nil {
		return Record{}, fmt.Errorf("decode background task %s: %w", id, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return Record{}, fmt.Errorf("decode background task %s: %w", id, err)
	}
	if err := validateRecord(record, id); err != nil {
		return Record{}, err
	}
	return record, nil
}

func validateRecord(record Record, id string) error {
	if record.Version != recordVersion {
		return fmt.Errorf("background task %s has unsupported record version %d", id, record.Version)
	}
	if record.ID != id {
		return fmt.Errorf("background task %s record ID is %q", id, record.ID)
	}
	if err := validateCommand(record.Command); err != nil {
		return fmt.Errorf("background task %s: %w", id, err)
	}
	if record.PID <= 0 || record.StartedAt.IsZero() || record.UpdatedAt.IsZero() || record.UpdatedAt.Before(record.StartedAt) {
		return fmt.Errorf("background task %s record has invalid producer metadata", id)
	}
	if record.Terminal != nil && record.Terminal.Kind != "final" && record.Terminal.Kind != "error" {
		return fmt.Errorf("background task %s has invalid terminal event kind %q", id, record.Terminal.Kind)
	}
	return nil
}

func validateID(id string) error {
	if len(id) < 20 || len(id) > 128 {
		return fmt.Errorf("invalid background task ID %q", id)
	}
	for _, char := range id {
		if (char < 'A' || char > 'Z') && (char < '2' || char > '7') {
			return fmt.Errorf("invalid background task ID %q", id)
		}
	}
	return nil
}

func validateCommand(command string) error {
	if command != "review" && command != "simplify" {
		return fmt.Errorf("invalid background task command %q", command)
	}
	return nil
}

func (s *Store) withRecordLock(id string, fn func(string) error) error {
	if err := validateID(id); err != nil {
		return err
	}
	lockPath := filepath.Join(s.dir, "."+id+".lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("lock background task %s: %w", id, err)
	}
	closeErr := lock.Close()
	if closeErr != nil {
		_ = os.Remove(lockPath)
		return fmt.Errorf("close background task %s lock: %w", id, closeErr)
	}
	defer os.Remove(lockPath)
	return fn(filepath.Join(s.dir, id+".json"))
}

func (s *Store) writeRecord(path string, record Record) error {
	temporary, err := os.CreateTemp(s.dir, ".record-*")
	if err != nil {
		return fmt.Errorf("create background task temporary record: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure background task temporary record: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(record); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("encode background task record: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync background task record: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close background task record: %w", err)
	}
	err = os.Rename(temporaryPath, path)
	if err != nil {
		return fmt.Errorf("publish background task record: %w", err)
	}
	if err := syncDirectory(s.dir); err != nil {
		return fmt.Errorf("sync background task directory: %w", err)
	}
	return nil
}
