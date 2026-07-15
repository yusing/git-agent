// Package background persists durable review and simplification task results.
package background

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/yusing/git-agent/internal/gitctx"
	"github.com/yusing/git-agent/internal/textutil"
	"github.com/yusing/git-agent/internal/trace"
)

const (
	legacyRecordVersion = 1
	recordVersion       = 2
	defaultPollInterval = 100 * time.Millisecond
	heartbeatInterval   = 5 * time.Second
	heartbeatStaleAfter = 30 * time.Second
	maxRecordBytes      = 4 << 20
	maxDiagnosticEvents = 8
	maxDiagnosticBytes  = 4 << 10
	maxDiagnosticLines  = 40
)

// Record is the versioned durable state of one background task.
type Record struct {
	Version   int                `json:"version"`
	ID        string             `json:"id"`
	Command   string             `json:"command"`
	PID       int                `json:"pid"`
	StartedAt time.Time          `json:"started_at"`
	UpdatedAt time.Time          `json:"updated_at"`
	Terminal  *trace.Event       `json:"terminal,omitempty"`
	Failure   *FailureDiagnostic `json:"failure,omitempty"`
}

type FailureDiagnostic struct {
	Model                 string                    `json:"model,omitempty"`
	Mode                  string                    `json:"mode,omitempty"`
	MaxSteps              int                       `json:"max_steps,omitempty"`
	MaxToolCalls          int                       `json:"max_tool_calls,omitempty"`
	RepositoryFingerprint *gitctx.ChangeFingerprint `json:"repository_fingerprint,omitempty"`
	ToolEvents            []FailureToolEvent        `json:"tool_events,omitempty"`
}

type FailureToolEvent struct {
	Seq       int    `json:"seq"`
	Kind      string `json:"kind"`
	Tool      string `json:"tool,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Payload   string `json:"payload,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

func (d *FailureDiagnostic) RecordToolEvent(event trace.Event) {
	if d == nil || (event.Kind != "tool-call" && event.Kind != "tool-output") {
		return
	}
	toolEvent := FailureToolEvent{
		Seq:    event.Seq,
		Kind:   event.Kind,
		Tool:   diagnosticString(event.Value["name"]),
		CallID: diagnosticString(event.Value["call_id"]),
	}
	if event.Kind == "tool-output" {
		toolEvent.Truncated, _ = event.Value["truncated"].(bool)
		toolEvent.Payload, toolEvent.Truncated = diagnosticToolOutput(event.Value["content"], toolEvent.Truncated)
	} else {
		toolEvent.Payload, toolEvent.Truncated = diagnosticToolCall(event.Value["arguments"])
	}
	if len(d.ToolEvents) == maxDiagnosticEvents {
		copy(d.ToolEvents, d.ToolEvents[1:])
		d.ToolEvents[len(d.ToolEvents)-1] = toolEvent
		return
	}
	d.ToolEvents = append(d.ToolEvents, toolEvent)
}

func diagnosticString(value any) string {
	text, _ := value.(string)
	limited, _ := textutil.Limit(text, 256, 1)
	return limited
}

func diagnosticToolCall(value any) (string, bool) {
	raw := diagnosticJSON(value)
	summary := diagnosticPayloadIdentity(raw)
	arguments, ok := diagnosticObject(value)
	if !ok {
		payload, truncated := marshalDiagnosticSummary(summary)
		return payload, truncated || len(raw) > 0
	}
	safeKeys := map[string]bool{
		"path": true, "paths": true, "source": true, "line_start": true, "line_end": true,
		"offset": true, "limit": true, "max_bytes": true, "max_lines": true, "max_matches": true,
		"glob": true, "type": true,
	}
	omitted := false
	for key, item := range arguments {
		if safeKeys[key] {
			summary[key] = item
		} else {
			omitted = true
		}
	}
	payload, truncated := marshalDiagnosticSummary(summary)
	return payload, omitted || truncated
}

func diagnosticToolOutput(value any, alreadyTruncated bool) (string, bool) {
	raw := diagnosticJSON(value)
	summary := diagnosticPayloadIdentity(raw)
	envelope, ok := diagnosticObject(value)
	if !ok {
		payload, truncated := marshalDiagnosticSummary(summary)
		return payload, alreadyTruncated || truncated || len(raw) > 0
	}
	included := map[string]bool{"ok": true, "tool": true, "truncated": true, "error": true}
	for _, key := range []string{"ok", "tool", "truncated"} {
		if item, exists := envelope[key]; exists {
			summary[key] = item
		}
	}
	if message, ok := envelope["error"].(string); ok {
		summary["error"] = diagnosticString(message)
	}
	if data, ok := envelope["data"].(map[string]any); ok {
		included["data"] = true
		keys := make([]string, 0, len(data))
		for key := range data {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		summary["data_keys"] = keys
	}
	omitted := false
	for key := range envelope {
		if !included[key] || key == "data" {
			omitted = true
		}
	}
	payload, truncated := marshalDiagnosticSummary(summary)
	return payload, alreadyTruncated || omitted || truncated
}

func diagnosticObject(value any) (map[string]any, bool) {
	if mapped, ok := value.(map[string]any); ok {
		return mapped, true
	}
	text, ok := value.(string)
	if !ok {
		return nil, false
	}
	var mapped map[string]any
	if json.Unmarshal([]byte(text), &mapped) != nil {
		return nil, false
	}
	return mapped, true
}

func diagnosticJSON(value any) []byte {
	if text, ok := value.(string); ok {
		return []byte(text)
	}
	data, _ := json.Marshal(value)
	return data
}

func diagnosticPayloadIdentity(raw []byte) map[string]any {
	sum := sha256.Sum256(raw)
	return map[string]any{"bytes": len(raw), "sha256": fmt.Sprintf("%x", sum)}
}

func marshalDiagnosticSummary(summary map[string]any) (string, bool) {
	data, err := json.Marshal(summary)
	if err != nil {
		return "", true
	}
	return textutil.Limit(string(data), maxDiagnosticBytes, maxDiagnosticLines)
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
	return storeAt(dir), nil
}

// FindStore locates a task record across project metadata directories.
func FindStore(metadataRoot, id string) (*Store, error) {
	if err := validateID(id); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(metadataRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("unknown background task %s", id)
	}
	if err != nil {
		return nil, fmt.Errorf("read background metadata root: %w", err)
	}
	var matches []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(metadataRoot, entry.Name(), "background")
		info, err := os.Lstat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect background store %s: %w", entry.Name(), err)
		}
		if !info.IsDir() {
			continue
		}
		if _, err := os.Lstat(filepath.Join(dir, id+".json")); err == nil {
			matches = append(matches, dir)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("inspect background task %s: %w", id, err)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("unknown background task %s", id)
	}
	if len(matches) > 1 {
		return nil, fmt.Errorf("background task %s exists in multiple project stores", id)
	}
	return storeAt(matches[0]), nil
}

func storeAt(dir string) *Store {
	return &Store{
		dir:          dir,
		pollInterval: defaultPollInterval,
		processAlive: processIsAlive,
		now:          time.Now,
	}
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
func (s *Store) Complete(id string, terminal trace.Event, failure *FailureDiagnostic, now time.Time) error {
	if terminal.Kind != "final" && terminal.Kind != "error" {
		return fmt.Errorf("background terminal event kind %q is invalid", terminal.Kind)
	}
	if terminal.Kind == "final" && failure != nil {
		return errors.New("successful background task cannot contain failure diagnostics")
	}
	failure = boundedFailureDiagnostic(failure)
	if err := validateFailureDiagnostic(failure); err != nil {
		return err
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
		record.Version = recordVersion
		record.UpdatedAt = now.UTC()
		record.Terminal = &terminal
		record.Failure = failure
		return s.writeRecord(path, record)
	})
}

func boundedFailureDiagnostic(diagnostic *FailureDiagnostic) *FailureDiagnostic {
	if diagnostic == nil {
		return nil
	}
	bounded := *diagnostic
	bounded.Model = diagnosticString(diagnostic.Model)
	bounded.Mode = diagnosticString(diagnostic.Mode)
	if diagnostic.RepositoryFingerprint != nil {
		fingerprint := *diagnostic.RepositoryFingerprint
		bounded.RepositoryFingerprint = &fingerprint
	}
	start := max(0, len(diagnostic.ToolEvents)-maxDiagnosticEvents)
	bounded.ToolEvents = append([]FailureToolEvent(nil), diagnostic.ToolEvents[start:]...)
	for index := range bounded.ToolEvents {
		event := &bounded.ToolEvents[index]
		event.Tool = diagnosticString(event.Tool)
		event.CallID = diagnosticString(event.CallID)
		payload, truncated := textutil.Limit(event.Payload, maxDiagnosticBytes, maxDiagnosticLines)
		event.Payload = payload
		event.Truncated = event.Truncated || truncated
	}
	return &bounded
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
	if record.Version != legacyRecordVersion && record.Version != recordVersion {
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
	if record.Version == legacyRecordVersion && record.Failure != nil {
		return fmt.Errorf("background task %s legacy record contains failure diagnostics", id)
	}
	if record.Failure != nil && (record.Terminal == nil || record.Terminal.Kind != "error") {
		return fmt.Errorf("background task %s has diagnostics without terminal error", id)
	}
	if err := validateFailureDiagnostic(record.Failure); err != nil {
		return fmt.Errorf("background task %s: %w", id, err)
	}
	return nil
}

func validateFailureDiagnostic(diagnostic *FailureDiagnostic) error {
	if diagnostic == nil {
		return nil
	}
	if len(diagnostic.Model) > 512 || len(diagnostic.Mode) > 512 {
		return errors.New("background failure diagnostic identity exceeds limits")
	}
	if diagnostic.MaxSteps < 0 || diagnostic.MaxToolCalls < 0 {
		return errors.New("background failure diagnostic budgets must be non-negative")
	}
	if len(diagnostic.ToolEvents) > maxDiagnosticEvents {
		return fmt.Errorf("background failure diagnostic exceeds %d tool events", maxDiagnosticEvents)
	}
	if fingerprint := diagnostic.RepositoryFingerprint; fingerprint != nil {
		if len(fingerprint.BaseTree) > 128 || len(fingerprint.TargetTree) > 128 || len(fingerprint.DirtySubmodules) > 128 {
			return errors.New("background failure diagnostic fingerprint exceeds limits")
		}
	}
	for _, event := range diagnostic.ToolEvents {
		if event.Kind != "tool-call" && event.Kind != "tool-output" {
			return fmt.Errorf("background failure diagnostic event kind %q is invalid", event.Kind)
		}
		if len(event.Tool) > 512 || len(event.CallID) > 512 || len(event.Payload) > maxDiagnosticBytes+256 {
			return errors.New("background failure diagnostic event exceeds limits")
		}
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
