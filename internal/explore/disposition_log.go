package explore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DispositionLog appends owner-only batch disposition records for one project.
type DispositionLog struct {
	path      string
	projectID string
}

// NewDispositionLog resolves the per-project log beneath XDG state.
func NewDispositionLog(projectID string) (*DispositionLog, error) {
	if !validProjectID(projectID) {
		return nil, fmt.Errorf("invalid project ID %q", projectID)
	}
	stateHome := strings.TrimSpace(os.Getenv("XDG_STATE_HOME"))
	if stateHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		stateHome = filepath.Join(home, ".local", "state")
	}
	return &DispositionLog{
		path:      filepath.Join(stateHome, "git-agent", projectID, "explore.log"),
		projectID: projectID,
	}, nil
}

// AppendBatch records one line per item after the batch is sealed. Logging is
// best-effort at the coordinator boundary so it never fails an exploration.
func (l *DispositionLog) AppendBatch(ctx context.Context, batchID, workspace string, parent *Session, items []BatchItem) (err error) {
	if l == nil || len(items) == 0 {
		return nil
	}
	appDir := filepath.Dir(filepath.Dir(l.path))
	projectDir := filepath.Dir(l.path)
	for _, dir := range []string{appDir, projectDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create explore log directory: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("secure explore log directory: %w", err)
		}
	}
	lock, err := lockCoordinator(ctx, l.path+".lock", defaultPollInterval)
	if err != nil {
		return fmt.Errorf("lock explore log: %w", err)
	}
	defer func() { err = errors.Join(err, lock.Unlock()) }()
	if err := os.Chmod(l.path+".lock", 0o600); err != nil {
		return fmt.Errorf("secure explore log lock: %w", err)
	}

	mode := "unbatched"
	if len(items) > 1 {
		mode = "batched"
	}
	parentID := ""
	depth := 0
	if parent != nil {
		parentID = parent.ID
		depth = parent.Depth + 1
	}
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	var records strings.Builder
	for _, item := range items {
		fmt.Fprintf(
			&records,
			"%s mode=%s branch=%t project_id=%s workspace=%q batch=%s size=%d item=%s parent=%s depth=%d query=[redacted]\n",
			timestamp, mode, parent != nil, l.projectID, workspace, batchID, len(items), item.ID, parentID, depth,
		)
	}
	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open explore log: %w", err)
	}
	if err := os.Chmod(l.path, 0o600); err != nil {
		return errors.Join(fmt.Errorf("secure explore log: %w", err), file.Close())
	}
	if _, err := io.WriteString(file, records.String()); err != nil {
		return errors.Join(fmt.Errorf("append explore log: %w", err), file.Close())
	}
	return errors.Join(file.Sync(), file.Close())
}

func validProjectID(projectID string) bool {
	if len(projectID) != 64 {
		return false
	}
	for _, char := range projectID {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
