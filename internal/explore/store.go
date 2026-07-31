// Package explore owns synchronous codebase-exploration sessions and batching.
package explore

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/bytedance/sonic"
	"github.com/yusing/git-agent/internal/openai"
)

const (
	sessionVersion  = 1
	MaxFollowUps    = 3
	maxSessionBytes = 64 << 20
)

// Session is one independently addressable branch of an exploration chain.
type Session struct {
	Version  int           `json:"version"`
	ID       string        `json:"id"`
	ParentID string        `json:"parent_id,omitempty"`
	Depth    int           `json:"depth"`
	Answer   string        `json:"answer"`
	History  []openai.Item `json:"history"`
}

// Store owns owner-only explore state beneath one project metadata directory.
type Store struct {
	sessionDir string
	batchDir   string
}

func NewStore(metadataDir string) (*Store, error) {
	dir := filepath.Join(metadataDir, "explore")
	sessionDir := filepath.Join(dir, "sessions")
	batchDir := filepath.Join(dir, "batches")
	for _, path := range []string{dir, sessionDir, batchDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, fmt.Errorf("create explore state directory: %w", err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			return nil, fmt.Errorf("secure explore state directory: %w", err)
		}
	}
	return &Store{sessionDir: sessionDir, batchDir: batchDir}, nil
}

func (s *Store) Read(id string) (Session, error) {
	if err := validateID(id); err != nil {
		return Session{}, err
	}
	path := filepath.Join(s.sessionDir, id+".json")
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Session{}, fmt.Errorf("unknown explore search ID %s", id)
	}
	if err != nil {
		return Session{}, fmt.Errorf("open explore session %s: %w", id, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxSessionBytes+1))
	if err != nil {
		return Session{}, fmt.Errorf("read explore session %s: %w", id, err)
	}
	if len(data) > maxSessionBytes {
		return Session{}, fmt.Errorf("explore session %s exceeds %d bytes", id, maxSessionBytes)
	}
	var session Session
	if err := sonic.ConfigStd.Unmarshal(data, &session); err != nil {
		return Session{}, fmt.Errorf("decode explore session %s: %w", id, err)
	}
	if err := validateSession(session); err != nil {
		return Session{}, fmt.Errorf("invalid explore session %s: %w", id, err)
	}
	if session.ID != id {
		return Session{}, fmt.Errorf("explore session ID mismatch: file %s contains %s", id, session.ID)
	}
	return session, nil
}

// FollowUpParent returns the replayable parent while its branch still has a
// context-preserving turn available. A nil parent means the caller must reset
// to a fresh search.
func (s *Store) FollowUpParent(id string) (*Session, error) {
	session, err := s.Read(id)
	if err != nil {
		return nil, err
	}
	if session.Depth >= MaxFollowUps {
		return nil, nil
	}
	return &session, nil
}

func (s *Store) create(session Session) error {
	if err := validateSession(session); err != nil {
		return err
	}
	encoded, err := sonic.ConfigStd.Marshal(session)
	if err != nil {
		return fmt.Errorf("encode explore session %s: %w", session.ID, err)
	}
	if len(encoded) > maxSessionBytes {
		return fmt.Errorf("explore session %s exceeds %d bytes", session.ID, maxSessionBytes)
	}
	path := filepath.Join(s.sessionDir, session.ID+".json")
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("explore session %s already exists", session.ID)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect explore session %s: %w", session.ID, err)
	}
	return writeJSONAtomic(s.sessionDir, path, session)
}

func newID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate explore search ID: %w", err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:]), nil
}

func validateID(id string) error {
	if len(id) != 26 {
		return fmt.Errorf("invalid explore search ID %q", id)
	}
	for _, char := range id {
		if (char < 'A' || char > 'Z') && (char < '2' || char > '7') {
			return fmt.Errorf("invalid explore search ID %q", id)
		}
	}
	return nil
}

func validateSession(session Session) error {
	if session.Version != sessionVersion {
		return fmt.Errorf("unsupported version %d", session.Version)
	}
	if err := validateID(session.ID); err != nil {
		return err
	}
	if session.ParentID != "" {
		if err := validateID(session.ParentID); err != nil {
			return fmt.Errorf("invalid parent ID: %w", err)
		}
	}
	if session.Depth < 0 || session.Depth > MaxFollowUps {
		return fmt.Errorf("invalid follow-up depth %d", session.Depth)
	}
	if session.Depth == 0 && session.ParentID != "" {
		return errors.New("initial explore session cannot have a parent")
	}
	if session.Depth > 0 && session.ParentID == "" {
		return errors.New("follow-up explore session requires a parent")
	}
	if strings.TrimSpace(session.Answer) == "" {
		return errors.New("explore session requires an answer")
	}
	if len(session.History) == 0 {
		return errors.New("explore session requires conversation history")
	}
	return nil
}

func writeJSONAtomic(dir, path string, value any) error {
	temporary, err := os.CreateTemp(dir, ".explore-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	encoder := sonic.ConfigStd.NewEncoder(temporary)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
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
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
